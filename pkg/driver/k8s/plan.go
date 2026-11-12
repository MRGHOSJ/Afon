package k8s

import (
	"context"
	"fmt"

	"github.com/MRGHOSJ/afon/pkg/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Plan diffs desired objects against live state, scoped to owner.
// Read-only by construction: classification rides dry-run applies
// (which never persist) and prune — skipped entirely when the driver
// runs with prune off (--no-prune) — is a label-scoped LIST. Order is
// sortDiffs' — dependency order with deletes last — which is also the
// order Apply will execute.
func (d *Driver) Plan(ctx context.Context, owner driver.Owner, desired []driver.Object) ([]driver.Diff, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.checkNamespace(ctx); err != nil {
		return nil, err
	}

	items := make([]prepared, 0, len(desired))
	for _, o := range desired {
		gvr, body, err := d.prepare(owner, o)
		if err != nil {
			return nil, err
		}
		items = append(items, prepared{obj: o, gvr: gvr, body: body})
	}

	diffs := make([]driver.Diff, 0, len(desired))
	for _, p := range items {
		df, err := d.classify(ctx, owner, p)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, df)
	}
	if d.prune {
		pruned, err := d.pruneDiffs(ctx, owner, items)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, pruned...)
	}
	sortDiffs(diffs)
	return diffs, nil
}

// prepared is one desired object after resolution: identity, API
// resource, and the exact body SSA would send.
type prepared struct {
	obj  driver.Object
	gvr  schema.GroupVersionResource
	body map[string]any
}

// classify turns one desired object into its diff:
//
//   - absent            → create (dry-run first, so admission denials
//     and name collisions fail the plan, not the apply)
//   - owned by another  → ErrForeignOwner, never adoption
//   - dry-run == live   → none
//   - dry-run != live   → update
//
// A cross-manager conflict during either dry-run surfaces as
// driver.ErrConflict — the spike proved dry-run and apply return the
// identical message, so plan and apply never disagree about force.
func (d *Driver) classify(ctx context.Context, owner driver.Owner, p prepared) (driver.Diff, error) {
	live, err := d.dyn.Resource(p.gvr).Namespace(d.namespace).Get(ctx, p.obj.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := d.ssa(ctx, p.gvr, p.body, true); err != nil {
			return driver.Diff{}, patchErr(p.obj, "dry-run apply", err)
		}
		return driver.Diff{Object: p.obj, Op: driver.OpCreate}, nil
	case err != nil:
		return driver.Diff{}, fmt.Errorf("get %s/%s: %w", p.obj.Kind, p.obj.Name, err)
	}
	if f, foreign := foreignOwner(live, owner); foreign {
		return driver.Diff{}, &driver.ErrForeignOwner{Flow: f.Flow, Node: f.Node, Object: p.obj}
	}
	dry, err := d.ssa(ctx, p.gvr, p.body, true)
	if err != nil {
		return driver.Diff{}, patchErr(p.obj, "dry-run apply", err)
	}
	op := driver.OpUpdate
	if sameState(dry.Object, live.Object) {
		op = driver.OpNone
	}
	return driver.Diff{Object: p.obj, Op: op}, nil
}

// pruneDiffs lists every kind the desired set touches — the documented
// orphan gap is that kinds absent from desired are never listed
// (Phase 4 inventory closes it) — scoped by owner labels, and turns
// owned-but-undesired objects into delete diffs. The selector narrows
// server-side; ownedBy re-verifies client-side, so an object is never
// deleted on an unverified claim. A kind the server no longer serves
// (CRD uninstalled) lists as nothing. Plan skips this entirely when
// the driver runs with prune off (--no-prune).
func (d *Driver) pruneDiffs(ctx context.Context, owner driver.Owner, items []prepared) ([]driver.Diff, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s", flowLabel, owner.Flow, nodeLabel, owner.Node)
	desired := map[schema.GroupVersionResource]map[string]bool{}
	for _, p := range items {
		if desired[p.gvr] == nil {
			desired[p.gvr] = map[string]bool{}
		}
		desired[p.gvr][p.obj.Name] = true
	}

	var out []driver.Diff
	listed := map[schema.GroupVersionResource]bool{}
	for _, p := range items {
		if listed[p.gvr] {
			continue
		}
		listed[p.gvr] = true
		list, err := d.dyn.Resource(p.gvr).Namespace(d.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("listing %s for prune: %w", p.gvr.Resource, err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			if !ownedBy(item, owner) || desired[p.gvr][item.GetName()] {
				continue
			}
			out = append(out, driver.Diff{Object: identityOf(item), Op: driver.OpDelete})
		}
	}
	return out, nil
}
