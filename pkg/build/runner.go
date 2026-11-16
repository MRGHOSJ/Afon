package build

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Runner executes throwaway build Jobs against a cluster. It is the
// only cluster client pkg/build needs; cmd/afon constructs one from
// the driver's rest.Config and hands it to image-build via the
// engine's ExecutorOptions.Cluster. The Runner never touches Secrets
// (locked decision: no Secret get/list anywhere in Afon) — credential
// problems surface as pod FailedMount events instead.
type Runner struct {
	client       kubernetes.Interface
	pollInterval time.Duration
}

// NewRunner wraps a typed clientset. Polling (not watch) keeps the
// primitive deterministic under fake clientsets and needs only
// get/list RBAC.
func NewRunner(client kubernetes.Interface) *Runner {
	return &Runner{client: client, pollInterval: 2 * time.Second}
}

// RunJob creates job in ns, waits for its pod to finish, and ALWAYS
// deletes the Job afterwards — success, failure, timeout, or
// cancellation — using a fresh context so Ctrl-C cannot skip the
// delete (ttlSecondsAfterFinished remains the crash safety net). On
// success it returns the termination message of messageContainer
// (image-build reads the build digest from it); on failure it returns
// an error enriched with pod/job events so mount and admission
// problems are legible without kubectl.
func (r *Runner) RunJob(ctx context.Context, ns string, job *batchv1.Job, messageContainer string) (string, error) {
	if err := r.ensureNamespace(ctx, ns); err != nil {
		return "", err
	}
	created, err := r.client.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", r.wrapCreate(ns, err)
	}
	defer r.deleteAlways(ns, created.Name)

	runID := created.Labels[RunIDLabel]
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	lastPod := ""

	for {
		select {
		case <-ctx.Done():
			return "", r.stuckError(ctx.Err(), ns, created.Name, lastPod)
		case <-ticker.C:
		}

		j, err := r.client.BatchV1().Jobs(ns).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return "", r.stuckError(ctx.Err(), ns, created.Name, lastPod)
			}
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("build job %q was deleted while running", created.Name)
			}
			return "", fmt.Errorf("reading build job %q: %w", created.Name, err)
		}
		pods, err := r.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: labels.Set{RunIDLabel: runID}.String(),
		})
		if err != nil {
			if ctx.Err() != nil {
				return "", r.stuckError(ctx.Err(), ns, created.Name, lastPod)
			}
			return "", fmt.Errorf("listing build pods: %w", err)
		}
		if len(pods.Items) > 0 {
			pod := pods.Items[0]
			lastPod = pod.Name

			// Success: the message container terminated cleanly.
			if cs := containerStatus(pod, messageContainer); cs != nil &&
				cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
				return cs.State.Terminated.Message, nil
			}
			// An init container that exited non-zero failed the pod
			// before the build started. Inspect pods before the Job
			// condition so this detail is not masked by
			// BackoffLimitExceeded.
			for _, cs := range pod.Status.InitContainerStatuses {
				if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
					if msg := strings.TrimSpace(t.Message); msg != "" {
						return "", r.withEvents(ns, pod.Name, fmt.Errorf(
							"build failed: init container %q exited with code %d: %s", cs.Name, t.ExitCode, msg))
					}
					return "", r.withEvents(ns, pod.Name, fmt.Errorf(
						"build failed: init container %q exited with code %d", cs.Name, t.ExitCode))
				}
			}
			// Any container that exited non-zero fails the Job.
			for _, cs := range pod.Status.ContainerStatuses {
				if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
					msg := strings.TrimSpace(t.Message)
					if msg == "" {
						msg = fmt.Sprintf("container %q exited with code %d", cs.Name, t.ExitCode)
					}
					return "", r.withEvents(ns, pod.Name, fmt.Errorf("build failed: %s", msg))
				}
			}
			// Pull errors never resolve on their own: fail fast with the
			// image name instead of waiting out the timeout.
			for _, cs := range pod.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil &&
					(w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") {
					return "", fmt.Errorf("pulling image for container %q: %s: %s", cs.Name, w.Reason, w.Message)
				}
			}
			if pod.Status.Phase == corev1.PodFailed {
				return "", r.withEvents(ns, pod.Name, fmt.Errorf("build pod failed: %s", pod.Status.Reason))
			}
		}
		// No pod detail to report (or none that failed): the Job's own
		// condition is the verdict — deadline, backoff, or a pod the
		// controller already swept away.
		if cond, failed := jobCondition(j, batchv1.JobFailed); failed {
			return "", r.withEvents(ns, created.Name, fmt.Errorf(
				"build job failed (%s): %s", cond.Reason, cond.Message))
		}
	}
}

// wrapCreate turns an admission refusal into a directive: when the
// message mentions pod-security mechanics, name the knob the operator
// must turn. Afon never modifies namespace labels itself.
func (r *Runner) wrapCreate(ns string, err error) error {
	base := fmt.Errorf("create build job in namespace %q: %w", ns, err)
	if !mentionsPodSecurity(err.Error()) {
		return base
	}
	return fmt.Errorf("%w (admission rejected the pod security settings; the namespace needs an appropriate "+
		"pod-security.kubernetes.io/enforce label for the build pod — Afon never modifies namespace labels)", base)
}

// baselineLabels are the PSA-baseline labels design.md requires on a
// build namespace (kaniko's posture is baseline-admitted).
var baselineLabels = map[string]string{
	"pod-security.kubernetes.io/enforce": "baseline",
	"pod-security.kubernetes.io/audit":   "baseline",
	"pod-security.kubernetes.io/warn":    "baseline",
}

// ensureNamespace creates ns with the baseline PSA labels when it does
// not exist, so a fresh cluster needs no manual kubectl setup. An
// existing namespace is never modified (Afon never edits namespace
// labels — a pre-existing mislabeled namespace still surfaces the
// admission hint in wrapCreate). AlreadyExists from a concurrent
// create is success; a forbidden create is a directive, not a raw API
// error.
func (r *Runner) ensureNamespace(ctx context.Context, ns string) error {
	_, err := r.client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking build namespace %q: %w", ns, err)
	}
	_, err = r.client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: baselineLabels},
	}, metav1.CreateOptions{})
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return fmt.Errorf("build namespace %q does not exist and afon cannot create it: %w "+
			"(create it manually: kubectl create namespace %s)", ns, err, ns)
	}
	return fmt.Errorf("creating build namespace %q: %w", ns, err)
}

func mentionsPodSecurity(msg string) bool {
	m := strings.ToLower(msg)
	for _, kw := range []string{"seccomp", "apparmor", "podsecurity", "pod-security"} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	return false
}

// stuckError reports a wait cut short by the context (deadline or
// cancellation): which job, the last pod state if known, and that
// pod's events (mount failures such as a missing or wrong-shaped
// Secret live there — Afon reads events, never Secrets).
func (r *Runner) stuckError(cause error, ns, jobName, podName string) error {
	base := fmt.Errorf("build job %q: %w", jobName, cause)
	return r.withEvents(ns, podName, base)
}

// withEvents appends deduplicated events for object (job or pod) to
// the error. Enrichment only: fetch failures never mask the base
// error, and the lookup uses a fresh context because the caller's may
// already be cancelled.
func (r *Runner) withEvents(ns, object string, base error) error {
	if object == "" {
		return base
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	list, err := r.client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + object,
	})
	if err != nil || len(list.Items) == 0 {
		return base
	}
	var seen []string
	var parts []string
	total := 0
	for _, ev := range list.Items {
		// The fake clientset ignores field selectors; filter here so
		// mixed-namespace/mixed-object events never leak into errors.
		if ev.InvolvedObject.Name != object {
			continue
		}
		line := strings.TrimSpace(ev.Reason + ": " + ev.Message)
		if line == "" || containsString(seen, line) {
			continue
		}
		seen = append(seen, line)
		if total+len(line) > 700 {
			break
		}
		parts = append(parts, line)
		total += len(line)
	}
	if len(parts) == 0 {
		return base
	}
	return fmt.Errorf("%w; events: %s", base, strings.Join(parts, "; "))
}

// deleteAlways is the always-delete guarantee: a fresh context with
// its own 30s budget, independent of the cancelled caller.
func (r *Runner) deleteAlways(ns, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	policy := metav1.DeletePropagationBackground
	_ = r.client.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
}

// SweepJobs deletes every afon-managed Job in ns matching
// labelSelector and returns how many it removed. It is a helper, not
// an automatic step: Inputs carries no flow identity, so an
// auto-sweep could not be scoped safely against concurrent builds.
// Normal leaks are covered by ttlSecondsAfterFinished instead.
func SweepJobs(ctx context.Context, client kubernetes.Interface, ns, labelSelector string) (int, error) {
	list, err := client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return 0, fmt.Errorf("listing build jobs: %w", err)
	}
	deleted := 0
	policy := metav1.DeletePropagationBackground
	for i := range list.Items {
		j := &list.Items[i]
		if j.Labels[ManagedByLabel] != ManagedByValue {
			continue
		}
		if err := client.BatchV1().Jobs(ns).Delete(ctx, j.Name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return deleted, fmt.Errorf("deleting job %q: %w", j.Name, err)
		}
		deleted++
	}
	return deleted, nil
}

func jobCondition(job *batchv1.Job, kind batchv1.JobConditionType) (batchv1.JobCondition, bool) {
	for _, c := range job.Status.Conditions {
		if c.Type == kind && c.Status == corev1.ConditionTrue {
			return c, true
		}
	}
	return batchv1.JobCondition{}, false
}

func containerStatus(pod corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
