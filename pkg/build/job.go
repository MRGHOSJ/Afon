package build

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label and lifecycle constants shared by every Job Afon creates.
const (
	// ManagedByLabel/ManagedByValue identify Jobs for cleanup and
	// observability (standard recommended label).
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "afon"
	// RunIDLabel scopes one build run's Job and Pod for lookup and
	// sweep; the value is a fresh crypto/rand hex id per Run.
	RunIDLabel = "afon.dev/run-id"
	// JobTTLSeconds is the always-on leak safety net: if the afon
	// process dies before its deferred delete runs, the Job controller
	// reaps the finished Job (and pods) anyway.
	JobTTLSeconds int32 = 600
	// JobDeadlineSlackSeconds pads the node's own timeout so the
	// node's wait fails first with good detail; the Job deadline only
	// fires when afon itself is gone.
	JobDeadlineSlackSeconds int64 = 60
	// Container names inside the build pod: the git checkout runs as
	// an init container; kaniko runs as the main container and writes
	// the digest to its termination message.
	GitInitContainerName = "git-checkout"
	BuildContainerName   = "kaniko"
)

// JobParams is what the primitive needs to build the throwaway build
// Job: identity (name/run id/namespace), the node timeout, and the
// pod's containers and volumes (image-build supplies those).
type JobParams struct {
	Name      string
	RunID     string
	Namespace string
	Timeout   time.Duration
	Init      []corev1.Container
	Build     corev1.Container
	Volumes   []corev1.Volume
}

// newRunID returns a 16-hex-char id from crypto/rand (no new module;
// uniqueness only, not a secret).
func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// jobNameFor derives the Job name from the run id. Both are stable
// for one Run call: name afon-build-<16 hex> stays well under the
// 63-character DNS limit.
func jobNameFor(runID string) string {
	return "afon-build-" + runID
}

// newJob builds the Job skeleton Afon guarantees for every build:
// managed-by + run-id labels on both Job and pod template (the pod
// template copy is how pods are found — no reliance on controller
// labels), restartPolicy Never, backoffLimit 0 (a failed build is
// never retried silently), activeDeadlineSeconds = timeout + slack,
// and ttlSecondsAfterFinished as the leak net. Containers and volumes
// come from the caller; the primitive owns nothing about them.
func newJob(p JobParams) *batchv1.Job {
	ttl := JobTTLSeconds
	deadline := int64(p.Timeout/time.Second) + JobDeadlineSlackSeconds
	backoff := int32(0)
	labels := map[string]string{
		ManagedByLabel: ManagedByValue,
		RunIDLabel:     p.RunID,
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			BackoffLimit:            &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:  corev1.RestartPolicyNever,
					InitContainers: p.Init,
					Containers:     []corev1.Container{p.Build},
					Volumes:        p.Volumes,
				},
			},
		},
	}
}
