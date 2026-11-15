package build

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNS = "afon-build"

func fastRunner(client *fake.Clientset) *Runner {
	r := NewRunner(client)
	r.pollInterval = 5 * time.Millisecond
	return r
}

func testJob(runID string) *batchv1.Job {
	return newJob(JobParams{
		Name:      jobNameFor(runID),
		RunID:     runID,
		Namespace: testNS,
		Timeout:   time.Minute,
		Build:     corev1.Container{Name: BuildContainerName, Image: "test-image"},
	})
}

// onJobCreate registers a create-jobs reactor that stores the Job via
// the tracker and optionally also stores pod (with the Job's run-id
// label copied in, exactly as the controller would keep them in
// sync). Returns the client.
func onJobCreate(t *testing.T, pod *corev1.Pod) *fake.Clientset {
	t.Helper()
	client := fake.NewClientset()
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		if err := client.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), job, job.Namespace); err != nil {
			return true, nil, err
		}
		if pod != nil {
			p := pod.DeepCopy()
			p.Namespace = job.Namespace
			if p.Labels == nil {
				p.Labels = map[string]string{}
			}
			p.Labels[RunIDLabel] = job.Labels[RunIDLabel]
			if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
				return true, nil, err
			}
		}
		return true, job, nil
	})
	return client
}

func succeededPod(message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "buildpod-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: BuildContainerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: message},
				},
			}},
		},
	}
}

func TestNewJobSpec(t *testing.T) {
	timeout := 20 * time.Minute
	build := corev1.Container{Name: "kaniko", Image: "img"}
	init := corev1.Container{Name: "git-checkout", Image: "git-img"}
	vol := corev1.Volume{Name: "workspace"}
	job := newJob(JobParams{
		Name: "afon-build-abc", RunID: "abc", Namespace: testNS,
		Timeout: timeout, Init: []corev1.Container{init}, Build: build, Volumes: []corev1.Volume{vol},
	})

	if job.Labels[ManagedByLabel] != ManagedByValue || job.Labels[RunIDLabel] != "abc" {
		t.Errorf("job labels = %v", job.Labels)
	}
	if job.Spec.Template.Labels[ManagedByLabel] != ManagedByValue || job.Spec.Template.Labels[RunIDLabel] != "abc" {
		t.Errorf("pod template labels = %v", job.Spec.Template.Labels)
	}
	if got := *job.Spec.TTLSecondsAfterFinished; got != 600 {
		t.Errorf("ttl = %d, want 600", got)
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got != int64(timeout/time.Second)+60 {
		t.Errorf("activeDeadline = %d, want %d", got, int64(timeout/time.Second)+60)
	}
	if got := *job.Spec.BackoffLimit; got != 0 {
		t.Errorf("backoffLimit = %d, want 0", got)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", job.Spec.Template.Spec.RestartPolicy)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 1 || job.Spec.Template.Spec.InitContainers[0].Name != "git-checkout" {
		t.Errorf("init containers = %v", job.Spec.Template.Spec.InitContainers)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 || job.Spec.Template.Spec.Containers[0].Name != "kaniko" {
		t.Errorf("containers = %v", job.Spec.Template.Spec.Containers)
	}
	if len(job.Spec.Template.Spec.Volumes) != 1 {
		t.Errorf("volumes = %v", job.Spec.Template.Spec.Volumes)
	}
}

func TestNewRunIDAndJobName(t *testing.T) {
	a, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 16 || a == b {
		t.Errorf("run ids: %q %q (want 16 hex chars, distinct)", a, b)
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("run id %q not hex", a)
			break
		}
	}
	if name := jobNameFor(a); len(name) > 63 || !strings.HasPrefix(name, "afon-build-") {
		t.Errorf("job name %q invalid", name)
	}
}

func TestRunner_SuccessReturnsMessageAndDeletesJob(t *testing.T) {
	client := onJobCreate(t, succeededPod("sha256:cafe"))
	r := fastRunner(client)
	got, err := r.RunJob(context.Background(), testNS, testJob("run1"), BuildContainerName)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if got != "sha256:cafe" {
		t.Errorf("message = %q", got)
	}
	if _, err := client.BatchV1().Jobs(testNS).Get(context.Background(), jobNameFor("run1"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job should always be deleted, get err = %v", err)
	}
}

func TestRunner_FailedContainerFailsAndDeletes(t *testing.T) {
	pod := succeededPod("")
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1
	pod.Status.ContainerStatuses[0].State.Terminated.Message = "kaniko exploded"
	client := onJobCreate(t, pod)
	r := fastRunner(client)
	_, err := r.RunJob(context.Background(), testNS, testJob("run2"), BuildContainerName)
	if err == nil || !strings.Contains(err.Error(), "kaniko exploded") {
		t.Fatalf("err = %v, want kaniko exploded", err)
	}
	if _, err := client.BatchV1().Jobs(testNS).Get(context.Background(), jobNameFor("run2"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job should be deleted after failure, get err = %v", err)
	}
}

func TestRunner_InitFailureFailsFast(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "buildpod-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "git-checkout",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "fatal: repository not found"},
					},
				},
				{
					Name:  BuildContainerName,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
				},
			},
		},
	}
	client := onJobCreate(t, pod)
	r := fastRunner(client)
	_, err := r.RunJob(context.Background(), testNS, testJob("run3"), BuildContainerName)
	if err == nil || !strings.Contains(err.Error(), "fatal: repository not found") {
		t.Fatalf("err = %v, want git failure message", err)
	}
}

func TestRunner_ImagePullFailsFast(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "buildpod-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: BuildContainerName,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ErrImagePull", Message: "failed to pull image",
				}},
			}},
		},
	}
	client := onJobCreate(t, pod)
	r := fastRunner(client)
	_, err := r.RunJob(context.Background(), testNS, testJob("run4"), BuildContainerName)
	if err == nil || !strings.Contains(err.Error(), "ErrImagePull") {
		t.Fatalf("err = %v, want ErrImagePull", err)
	}
}

func TestRunner_CancelStillDeletesJob(t *testing.T) {
	client := onJobCreate(t, nil) // job only, pod never appears
	r := fastRunner(client)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(40*time.Millisecond, cancel)
	_, err := r.RunJob(ctx, testNS, testJob("run5"), BuildContainerName)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := client.BatchV1().Jobs(testNS).Get(context.Background(), jobNameFor("run5"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job should be deleted after cancel, get err = %v", err)
	}
}

func TestRunner_TimeoutSurfacesFailedMountEvent(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "buildpod-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  BuildContainerName,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}},
		},
	}
	client := onJobCreate(t, pod)
	if _, err := client.CoreV1().Events(testNS).Create(context.Background(), &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "mount-1", Namespace: testNS},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: testNS, Name: "buildpod-1",
		},
		Reason:  "FailedMount",
		Message: `secret "dockerhub-creds" not found`,
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r := fastRunner(client)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := r.RunJob(ctx, testNS, testJob("run6"), BuildContainerName)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "secret \"dockerhub-creds\" not found") ||
		!strings.Contains(err.Error(), "FailedMount") {
		t.Errorf("err should carry the FailedMount event, got: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err should wrap deadline exceeded, got: %v", err)
	}
}

func TestRunner_JobFailedConditionSurfacesEvents(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline",
		}}
		return true, job, client.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), job, job.Namespace)
	})
	name := jobNameFor("run7")
	if _, err := client.CoreV1().Events(testNS).Create(context.Background(), &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "jevent", Namespace: testNS},
		InvolvedObject: corev1.ObjectReference{Kind: "Job", Namespace: testNS, Name: name},
		Reason:         "DeadlineExceeded",
		Message:        "build exceeded active deadline",
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Also store a pod-free state: the job condition must fail the
	// wait even with no pods at all.
	r := fastRunner(client)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.RunJob(ctx, testNS, testJob("run7"), BuildContainerName)
	if err == nil || !strings.Contains(err.Error(), "Job was active longer than specified deadline") {
		t.Fatalf("err = %v, want deadline message", err)
	}
	// The failure branch reads events for the JOB object.
	if !strings.Contains(err.Error(), "build exceeded active deadline") {
		t.Errorf("err should carry job events, got: %v", err)
	}
}

func TestRunner_AdmissionSecurityHint(t *testing.T) {
	client := fake.NewClientset()
	denied := apierrors.NewForbidden(
		schema.GroupResource{Group: "batch", Resource: "jobs"},
		"afon-build-run8",
		errors.New(`pods "afon-build-run8" is forbidden: violates PodSecurity "restricted: seccompProfile (restricted)"`),
	)
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, denied
	})
	r := fastRunner(client)
	_, err := r.RunJob(context.Background(), testNS, testJob("run8"), BuildContainerName)
	if err == nil {
		t.Fatal("expected admission error")
	}
	if !strings.Contains(err.Error(), testNS) || !strings.Contains(err.Error(), "pod-security.kubernetes.io/enforce") {
		t.Errorf("err should name namespace and the label knob, got: %v", err)
	}
	if !strings.Contains(err.Error(), "never modifies namespace labels") {
		t.Errorf("err should state Afon never modifies namespaces, got: %v", err)
	}
}

func TestRunner_AdmissionPlainRbacNoHint(t *testing.T) {
	client := fake.NewClientset()
	denied := apierrors.NewForbidden(
		schema.GroupResource{Group: "batch", Resource: "jobs"},
		"afon-build-run9",
		errors.New(`jobs.batch is forbidden: user "dev" cannot create resource "jobs"`),
	)
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, denied
	})
	r := fastRunner(client)
	_, err := r.RunJob(context.Background(), testNS, testJob("run9"), BuildContainerName)
	if err == nil || !strings.Contains(err.Error(), testNS) {
		t.Fatalf("err = %v, want namespace", err)
	}
	if strings.Contains(err.Error(), "enforce") {
		t.Errorf("plain RBAC denial must not get a pod-security hint: %v", err)
	}
}

func TestSweepJobsByLabel(t *testing.T) {
	client := fake.NewClientset()
	mk := func(name, runID string) *batchv1.Job {
		j := newJob(JobParams{Name: name, RunID: runID, Namespace: testNS, Timeout: time.Minute,
			Build: corev1.Container{Name: "kaniko"}})
		return j
	}
	for _, j := range []*batchv1.Job{
		mk(jobNameFor("aaa1"), "aaa1"),
		mk(jobNameFor("bbb2"), "bbb2"),
	} {
		if _, err := client.BatchV1().Jobs(testNS).Create(context.Background(), j, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other-team-job", Namespace: testNS,
		Labels: map[string]string{"app": "theirs"}}}
	if _, err := client.BatchV1().Jobs(testNS).Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	deleted, err := SweepJobs(context.Background(), client, testNS, RunIDLabel+"=aaa1")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	list, err := client.BatchV1().Jobs(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, j := range list.Items {
		names[j.Name] = true
	}
	if names[jobNameFor("aaa1")] {
		t.Error("aaa1 should be swept")
	}
	if !names[jobNameFor("bbb2")] {
		t.Error("bbb2 must survive")
	}
	if !names["other-team-job"] {
		t.Error("foreign job must survive")
	}
}

func TestRunner_JobVanishedMidRun(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// Return the object without storing it: Get will 404.
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		return true, job, nil
	})
	r := fastRunner(client)
	_, err := r.RunJob(context.Background(), testNS, testJob("run10"), BuildContainerName)
	if err == nil || !strings.Contains(err.Error(), "deleted while running") {
		t.Fatalf("err = %v, want deleted while running", err)
	}
}
