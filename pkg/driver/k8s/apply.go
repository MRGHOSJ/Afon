package k8s

import (
	"context"
	"fmt"

	"github.com/MRGHOSJ/afon/pkg/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Apply writes diffs in sortDiffs order: creates and updates by
// dependency tier, deletes last in reverse tier. It is atomic as far
// as Kubernetes allows: every mutation is rehearsed first — a dry-run
// apply for creates and updates, a confirming GET for deletes — so any
// failure the server can predict (conflicts, admission, schema,
// missing delete target) happens before the first write. Only a race
// after rehearsal can leave a partial apply; re-running converges.
// Owner stamps the applied bodies; the diffs themselves are trusted —
// Plan produced them moments earlier.
func (d *Driver) Apply(ctx context.Context, owner driver.Owner, diffs []driver.Diff) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ordered := make([]driver.Diff, len(diffs))
	copy(ordered, diffs)
	sortDiffs(ordered)

	// Validate every mutation before the first write.
	type mutation struct {
		obj  driver.Object
		gvr  schema.GroupVersionResource
		body map[string]any // nil for deletes
		uid  types.UID      // set for deletes: the precondition
	}
	plan := make([]mutation, 0, len(ordered))
	for _, df := range ordered {
		switch df.Op {
		case driver.OpNone:
			continue
		case driver.OpCreate, driver.OpUpdate:
			gvr, body, err := d.prepare(owner, df.Object)
			if err != nil {
				return err
			}
			if _, err := d.ssa(ctx, gvr, body, true); err != nil {
				return patchErr(df.Object, "dry-run apply", err)
			}
			plan = append(plan, mutation{obj: df.Object, gvr: gvr, body: body})
		case driver.OpDelete:
			gvr, _, err := d.prepare(owner, df.Object)
			if err != nil {
				return err
			}
			live, err := d.dyn.Resource(gvr).Namespace(d.namespace).Get(ctx, df.Object.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("delete of absent object %s/%s", df.Object.Kind, df.Object.Name)
			}
			if err != nil {
				return fmt.Errorf("get %s/%s before delete: %w", df.Object.Kind, df.Object.Name, err)
			}
			plan = append(plan, mutation{obj: df.Object, gvr: gvr, uid: live.GetUID()})
		default:
			return fmt.Errorf("unsupported op %q", df.Op)
		}
	}

	// Execute. Rehearsal already passed, so these failures are races:
	// wrap with identity and let the caller re-run.
	for _, m := range plan {
		if m.body != nil {
			if _, err := d.ssa(ctx, m.gvr, m.body, false); err != nil {
				return patchErr(m.obj, "apply", err)
			}
			continue
		}
		prop := metav1.DeletePropagationBackground
		uid := m.uid
		err := d.dyn.Resource(m.gvr).Namespace(d.namespace).Delete(ctx, m.obj.Name, metav1.DeleteOptions{
			PropagationPolicy: &prop,
			// Pin the UID validation observed: if the object was
			// deleted and recreated between plan and this call, the
			// precondition refuses to delete the stranger.
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		if err != nil {
			return fmt.Errorf("delete %s/%s: %w", m.obj.Kind, m.obj.Name, err)
		}
	}
	return nil
}
