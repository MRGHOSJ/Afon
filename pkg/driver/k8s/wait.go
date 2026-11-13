package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// waitPollInterval is the pause between readiness checks. Tests shrink
// it so timeout paths run in milliseconds.
var waitPollInterval = 2 * time.Second

// Wait implements driver.Waiter: poll the live cluster until every
// tracked object in objs is ready, the context ends, timeout elapses,
// or an object fails or disappears. Objects whose group/kind is not
// waited on (Service, ConfigMap, anything unknown) are skipped without
// a GET — the readiness table below is matched by group+kind so a
// same-named kind in another group (a CRD) never collides.
//
// timeout <= 0 means no deadline: the caller's context bounds the wait.
func (d *Driver) Wait(ctx context.Context, owner driver.Owner, objs []driver.Object, timeout time.Duration) error {
	pending := make([]driver.Object, 0, len(objs))
	for _, o := range objs {
		if readinessOf(o) != nil {
			pending = append(pending, o)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	start := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		notReady := make([]string, 0, len(pending))
		for _, o := range pending {
			rule := readinessOf(o)
			u, err := d.getLive(ctx, o)
			if err != nil {
				return err
			}
			ready, obs, failed := rule(u.Object)
			if failed != nil {
				return failed
			}
			if !ready {
				id := o.Kind + "/" + o.Name
				if obs != "" {
					id += " (" + obs + ")"
				}
				notReady = append(notReady, id)
			}
		}
		if len(notReady) == 0 {
			return nil
		}
		if timeout > 0 && time.Since(start) >= timeout {
			return fmt.Errorf("timed out after %s waiting for %s",
				timeout.Round(time.Millisecond), strings.Join(notReady, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// getLive fetches the object's live form, defaulting the namespace to
// the driver's. A disappearance mid-wait is an error: the apply said
// the object exists, so a 404 means something else deleted it.
func (d *Driver) getLive(ctx context.Context, o driver.Object) (*unstructured.Unstructured, error) {
	mapping, err := d.mappingFor(o)
	if err != nil {
		return nil, fmt.Errorf("waiting for %s/%s: %w", o.Kind, o.Name, err)
	}
	ns := o.Namespace
	if ns == "" {
		ns = d.namespace
	}
	u, err := d.dyn.Resource(mapping.Resource).Namespace(ns).Get(ctx, o.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("waiting for %s/%s: object disappeared (not found)", o.Kind, o.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("waiting for %s/%s: %w", o.Kind, o.Name, err)
	}
	return u, nil
}

// readinessRule judges one live object. ready reports readiness, obs is
// the last observed readiness numbers for the timeout message, and
// failed is a terminal condition (a Job marked Failed) that aborts the
// wait immediately instead of burning the timeout.
type readinessRule func(obj map[string]any) (ready bool, obs string, failed error)

// readinessOf returns the rule for kinds Wait tracks, or nil for kinds
// it skips. Matched by group+kind — version is irrelevant to the status
// shape, and the group keeps a CRD's same-named kind out.
func readinessOf(o driver.Object) readinessRule {
	switch {
	case o.Group == "apps" && o.Kind == "Deployment":
		return deploymentReady
	case o.Group == "apps" && o.Kind == "StatefulSet":
		return statefulSetReady
	case o.Group == "apps" && o.Kind == "DaemonSet":
		return daemonSetReady
	case o.Group == "batch" && o.Kind == "Job":
		return jobReady
	case o.Group == "" && o.Kind == "Pod":
		return podReady
	default:
		return nil
	}
}

// deploymentReady: observedGeneration caught up, Available=True, and
// every updated replica is in place (spec.replicas defaults to 1, the
// API server's own default).
func deploymentReady(obj map[string]any) (bool, string, error) {
	observed := digInt(obj, "status", "observedGeneration")
	generation := digInt(obj, "metadata", "generation")
	desired := digInt(obj, "spec", "replicas")
	if desired == 0 {
		desired = 1 // API default when spec.replicas is unset
	}
	updated := digInt(obj, "status", "updatedReplicas")
	available := conditionStatus(obj, "Available")
	obs := fmt.Sprintf("observedGeneration=%d generation=%d available=%s replicas=%d updatedReplicas=%d",
		observed, generation, available, desired, updated)
	ready := observed >= generation && available == "True" && updated == desired
	return ready, obs, nil
}

func statefulSetReady(obj map[string]any) (bool, string, error) {
	observed := digInt(obj, "status", "observedGeneration")
	generation := digInt(obj, "metadata", "generation")
	desired := digInt(obj, "spec", "replicas")
	if desired == 0 {
		desired = 1
	}
	readyReplicas := digInt(obj, "status", "readyReplicas")
	updated := digInt(obj, "status", "updatedReplicas")
	obs := fmt.Sprintf("observedGeneration=%d generation=%d readyReplicas=%d updatedReplicas=%d replicas=%d",
		observed, generation, readyReplicas, updated, desired)
	ready := observed >= generation && readyReplicas == desired && updated == desired
	return ready, obs, nil
}

func daemonSetReady(obj map[string]any) (bool, string, error) {
	observed := digInt(obj, "status", "observedGeneration")
	generation := digInt(obj, "metadata", "generation")
	desired := digInt(obj, "status", "desiredNumberScheduled")
	numberReady := digInt(obj, "status", "numberReady")
	updated := digInt(obj, "status", "updatedNumberScheduled")
	obs := fmt.Sprintf("observedGeneration=%d generation=%d numberReady=%d updatedNumberScheduled=%d desiredNumberScheduled=%d",
		observed, generation, numberReady, updated, desired)
	ready := observed >= generation && numberReady == desired && updated == desired
	return ready, obs, nil
}

// jobReady: Complete=True is ready; Failed=True aborts the wait with
// the condition's message instead of timing out on a job that can
// never succeed.
func jobReady(obj map[string]any) (bool, string, error) {
	complete := conditionStatus(obj, "Complete")
	failed := conditionStatus(obj, "Failed")
	obs := fmt.Sprintf("complete=%s failed=%s", complete, failed)
	switch {
	case failed == "True":
		msg := conditionMessage(obj, "Failed")
		if msg == "" {
			msg = "condition Failed=True"
		}
		return false, obs, fmt.Errorf("Job/%s failed: %s", nestedString(obj, "metadata", "name"), msg)
	case complete == "True":
		return true, obs, nil
	default:
		return false, obs, nil
	}
}

// podReady: Succeeded is done; Running counts only when every container
// reports ready (an empty status list is never ready — nothing has
// reported yet).
func podReady(obj map[string]any) (bool, string, error) {
	phase := nestedString(obj, "status", "phase")
	statuses, _, _ := unstructured.NestedSlice(obj, "status", "containerStatuses")
	allReady := len(statuses) > 0
	for _, s := range statuses {
		m, ok := s.(map[string]any)
		if !ok || !digBool(m) {
			allReady = false
			break
		}
	}
	obs := fmt.Sprintf("phase=%s containersReady=%v", phase, allReady)
	ready := phase == "Succeeded" || (phase == "Running" && allReady)
	return ready, obs, nil
}

// conditionStatus returns the status of a status.conditions entry ("True",
// "False", "Missing"), or "Missing" when the condition is absent.
func conditionStatus(obj map[string]any, condType string) string {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == condType {
			if s, _ := m["status"].(string); s != "" {
				return s
			}
		}
	}
	return "Missing"
}

func conditionMessage(obj map[string]any, condType string) string {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == condType {
			msg, _ := m["message"].(string)
			return msg
		}
	}
	return ""
}

// digInt reads a nested numeric field. The live API hands back int64;
// hand-built fixtures may carry int or a whole-number float64 (JSON's
// number type), so every spelling of a whole number converts. Missing
// or non-numeric fields read as 0.
func digInt(obj map[string]any, fields ...string) int64 {
	v, ok, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if err != nil || !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	default:
		return 0
	}
}

func nestedString(obj map[string]any, fields ...string) string {
	v, ok, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if err != nil || !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func digBool(m map[string]any) bool {
	b, _ := m["ready"].(bool)
	return b
}
