package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// withFastPoll shrinks the poll interval so timeout paths finish in
// milliseconds; restored on cleanup.
func withFastPoll(t *testing.T) {
	t.Helper()
	prev := waitPollInterval
	waitPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { waitPollInterval = prev })
}

func waitObj(group, version, kind, name string) driver.Object {
	return driver.Object{Group: group, Version: version, Kind: kind, Namespace: testNS, Name: name}
}

func deployWaitObj(name string) driver.Object {
	return waitObj("apps", "v1", "Deployment", name)
}

// --- readiness fixtures -------------------------------------------------

func readyDeployment(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": testNS, "generation": int64(1)},
		"spec":     map[string]any{"replicas": int64(1)},
		"status": map[string]any{
			"observedGeneration": int64(1),
			"replicas":           int64(1),
			"updatedReplicas":    int64(1),
			"availableReplicas":  int64(1),
			"conditions": []any{map[string]any{
				"type": "Available", "status": "True",
			}},
		},
	}}
}

// notReadyDeployment: controller has not reconciled yet — the exact
// shape a just-applied Deployment has for the first poll or two.
func notReadyDeployment(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": testNS, "generation": int64(1)},
		"spec":     map[string]any{"replicas": int64(1)},
		"status": map[string]any{
			"observedGeneration": int64(0),
			"replicas":           int64(1),
			"updatedReplicas":    int64(0),
			"conditions": []any{map[string]any{
				"type": "Available", "status": "False", "message": "min replicas not available",
			}},
		},
	}}
}

func readyStatefulSet(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": name, "namespace": testNS, "generation": int64(1)},
		"spec":     map[string]any{"replicas": int64(2)},
		"status": map[string]any{
			"observedGeneration": int64(1),
			"readyReplicas":      int64(2),
			"updatedReplicas":    int64(2),
		},
	}}
}

func readyDaemonSet(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{"name": name, "namespace": testNS, "generation": int64(1)},
		"status": map[string]any{
			"observedGeneration":     int64(1),
			"desiredNumberScheduled": int64(3),
			"numberReady":            int64(3),
			"updatedNumberScheduled": int64(3),
		},
	}}
}

func readyJob(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": testNS, "generation": int64(1)},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type": "Complete", "status": "True",
			}},
		},
	}}
}

func failedJob(name, message string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": testNS, "generation": int64(1)},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type": "Failed", "status": "True", "message": message,
			}},
		},
	}}
}

func succeededPod(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": testNS},
		"status":   map[string]any{"phase": "Succeeded"},
	}}
}

func runningPod(name string, containersReady ...bool) *unstructured.Unstructured {
	statuses := make([]any, 0, len(containersReady))
	for _, r := range containersReady {
		statuses = append(statuses, map[string]any{"name": "app", "ready": r})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": testNS},
		"status": map[string]any{
			"phase":             "Running",
			"containerStatuses": statuses,
		},
	}}
}

// --- tests ---------------------------------------------------------------

// TestWait_ReadyKinds: every tracked kind returns nil the moment its
// readiness rule sees a ready live object — no sleep, no timeout.
func TestWait_ReadyKinds(t *testing.T) {
	cases := []struct {
		name string
		obj  driver.Object
		seed runtime.Object
	}{
		{"deployment", deployWaitObj("web"), readyDeployment("web")},
		{"statefulset", waitObj("apps", "v1", "StatefulSet", "db"), readyStatefulSet("db")},
		{"daemonset", waitObj("apps", "v1", "DaemonSet", "agent"), readyDaemonSet("agent")},
		{"job", waitObj("batch", "v1", "Job", "migrate"), readyJob("migrate")},
		{"pod succeeded", waitObj("", "v1", "Pod", "job-pod"), succeededPod("job-pod")},
		{"pod running all ready", waitObj("", "v1", "Pod", "srv"), runningPod("srv", true, true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDriver(t, false, nil, seedNamespace(), tc.seed)
			if err := d.Wait(context.Background(), testOwner, []driver.Object{tc.obj}, time.Second); err != nil {
				t.Errorf("Wait on ready %s: %v", tc.obj.Kind, err)
			}
		})
	}
}

// TestWait_DeploymentTimeout: a never-ready Deployment fails after the
// deadline with an error naming the object and the last observed
// readiness numbers — and it takes the timeout, not forever.
func TestWait_DeploymentTimeout(t *testing.T) {
	withFastPoll(t)
	d, _ := newTestDriver(t, false, nil, seedNamespace(), notReadyDeployment("web"))

	start := time.Now()
	err := d.Wait(context.Background(), testOwner, []driver.Object{deployWaitObj("web")}, 50*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Wait must time out on a never-ready Deployment")
	}
	for _, want := range []string{"timed out", "Deployment/web", "observedGeneration=0", "generation=1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error missing %q:\n%s", want, err)
		}
	}
	if elapsed < 40*time.Millisecond {
		t.Errorf("returned after %v, want to actually wait out the 50ms timeout", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("returned after %v, want ~50ms with a fast poll interval", elapsed)
	}
}

// TestWait_TimeoutNamesOnlyNotReady: a ready sibling in the same wait
// is not named — only the object(s) still failing appear.
func TestWait_TimeoutNamesOnlyNotReady(t *testing.T) {
	withFastPoll(t)
	d, _ := newTestDriver(t, false, nil, seedNamespace(),
		readyDeployment("good"), notReadyDeployment("bad"))
	err := d.Wait(context.Background(), testOwner,
		[]driver.Object{deployWaitObj("good"), deployWaitObj("bad")}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("want timeout")
	}
	if strings.Contains(err.Error(), "Deployment/good") {
		t.Errorf("ready object named in timeout: %s", err)
	}
	if !strings.Contains(err.Error(), "Deployment/bad") {
		t.Errorf("not-ready object missing from timeout: %s", err)
	}
}

// TestWait_JobFailed: Failed=True aborts immediately with the
// condition message — no burning the timeout on a job that can never
// become complete.
func TestWait_JobFailed(t *testing.T) {
	d, _ := newTestDriver(t, false, nil, seedNamespace(), failedJob("boom", "BackoffLimitExceeded"))
	start := time.Now()
	err := d.Wait(context.Background(), testOwner,
		[]driver.Object{waitObj("batch", "v1", "Job", "boom")}, 30*time.Second)
	if err == nil {
		t.Fatal("failed Job must error")
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("failed Job must fail fast, not time out: %s", err)
	}
	for _, want := range []string{"Job/boom failed", "BackoffLimitExceeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %s", want, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("failed Job took %v, want an immediate error", elapsed)
	}
}

// TestWait_SkipsUntrackedKinds: Service and ConfigMap are outside the
// readiness table — Wait succeeds without issuing a single GET for
// them (a CRD or an unconfigured kind behaves the same way).
func TestWait_SkipsUntrackedKinds(t *testing.T) {
	svc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "svc", "namespace": testNS},
	}}
	cm := appliedCM("cfg", testOwner, map[string]any{"k": "v"})
	d, dyn := newTestDriver(t, false, nil, seedNamespace(), svc, cm)

	objs := []driver.Object{
		waitObj("", "v1", "Service", "svc"),
		waitObj("", "v1", "ConfigMap", "cfg"),
		waitObj("example.com", "v1", "Widget", "w"), // unknown kind: skipped, never mapped
	}
	if err := d.Wait(context.Background(), testOwner, objs, 50*time.Millisecond); err != nil {
		t.Fatalf("Wait over skipped kinds: %v", err)
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() != "get" {
			continue
		}
		switch a.GetResource() {
		case svcGVR, cmGVR:
			t.Errorf("Wait GETed skipped kind %s", a.GetResource())
		}
	}
}

// TestWait_NotFound: an object that disappears mid-wait is an error
// naming it — never a silent success or a timeout.
func TestWait_NotFound(t *testing.T) {
	d, _ := newTestDriver(t, false, nil, seedNamespace())
	err := d.Wait(context.Background(), testOwner, []driver.Object{deployWaitObj("ghost")}, time.Minute)
	if err == nil {
		t.Fatal("missing object must fail the wait")
	}
	for _, want := range []string{"Deployment/ghost", "not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %s", want, err)
		}
	}
}

// TestWait_ZeroTimeoutCancelled: timeout 0 means no deadline — the
// wait runs until the context is cancelled, and the error is the
// context's, not a fabricated timeout.
func TestWait_ZeroTimeoutCancelled(t *testing.T) {
	withFastPoll(t)
	d, _ := newTestDriver(t, false, nil, seedNamespace(), notReadyDeployment("web"))

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(40*time.Millisecond, cancel)
	err := d.Wait(ctx, testOwner, []driver.Object{deployWaitObj("web")}, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("zero timeout must not fabricate a timeout error: %s", err)
	}
}

// TestWait_PodContainersNotReady: Running with an unready container is
// not ready — the wait keeps polling (here: times out fast).
func TestWait_PodContainersNotReady(t *testing.T) {
	withFastPoll(t)
	d, _ := newTestDriver(t, false, nil, seedNamespace(), runningPod("srv", false))
	err := d.Wait(context.Background(), testOwner,
		[]driver.Object{waitObj("", "v1", "Pod", "srv")}, 40*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "Pod/srv") {
		t.Fatalf("err = %v, want timeout naming Pod/srv", err)
	}
}

// TestWait_GetActionScoped: a tracked wait issues GETs on exactly the
// object's own GVR and namespace — no list, no wildcard.
func TestWait_GetActionScoped(t *testing.T) {
	d, dyn := newTestDriver(t, false, nil, seedNamespace(), readyDeployment("web"))
	if err := d.Wait(context.Background(), testOwner, []driver.Object{deployWaitObj("web")}, time.Second); err != nil {
		t.Fatal(err)
	}
	var gets int
	for _, a := range dyn.Actions() {
		if a.GetVerb() != "get" {
			continue
		}
		gets++
		if a.GetResource() != deployGVR {
			t.Errorf("GET on %s, want %s", a.GetResource(), deployGVR)
		}
		ga, ok := a.(k8stesting.GetAction)
		if !ok {
			t.Fatalf("get action has type %T", a)
		}
		if ga.GetName() != "web" || ga.GetNamespace() != testNS {
			t.Errorf("GET %s/%s, want %s/web", ga.GetNamespace(), ga.GetName(), testNS)
		}
	}
	if gets != 1 {
		t.Errorf("GET count = %d, want 1", gets)
	}
	// And nothing lists.
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "list" {
			t.Errorf("Wait listed %s; readiness is per-object GETs", a.GetResource())
		}
	}
}
