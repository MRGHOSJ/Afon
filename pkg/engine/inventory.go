package engine

import (
	"context"
	"fmt"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/release"
)

// pruneInventory returns the inventory of the newest recorded release
// that carries one. Records come back newest-first; pending records
// and failed runs that applied nothing carry no inventory, so a broken
// apply in between does not silence pruning — the newest record that
// actually names objects supplies them (a failed run that did apply
// carries its live inventory too: those objects are still the flow's
// to prune). It returns nil when nothing with inventory has been
// recorded yet.
func pruneInventory(ctx context.Context, backend release.Backend) ([]release.InventoryObject, error) {
	records, err := backend.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading release records: %w", err)
	}
	for _, r := range records {
		if len(r.Inventory) > 0 {
			return r.Inventory, nil
		}
	}
	return nil, nil
}

// inventoryPruneDiffs adds deletes for objects this node recorded in
// the release inventory but no longer desires — the kind-removal gap
// label pruning cannot close, because the driver only lists kinds some
// node still desires (design §4 inventory prune).
//
// Each candidate is verified live through ObjectReader: Read omits
// anything absent or foreign. That doubles as the dedupe against
// label-prune deletes already in planned — an object label pruning
// already covers reads absent here — and, for candidates that survive,
// A6 keeps exactly one delete per object by dropping the planned
// duplicates and appending the inventory's live-identity version. A
// driver without Read verifies nothing and adds nothing.
func inventoryPruneDiffs(ctx context.Context, backend release.Backend, d driver.Driver, owner driver.Owner, desired []driver.Object, planned []driver.Diff) ([]driver.Diff, error) {
	inv, err := pruneInventory(ctx, backend)
	if err != nil || len(inv) == 0 {
		return planned, err
	}
	desiredKeys := make(map[string]bool, len(desired))
	for _, o := range desired {
		desiredKeys[o.Key()] = true
	}
	var candidates []driver.Object
	seen := make(map[string]bool, len(inv))
	for _, e := range inv {
		if e.Node != owner.Node {
			continue
		}
		o := driver.Object{Group: e.Group, Version: e.Version, Kind: e.Kind, Namespace: e.Namespace, Name: e.Name}
		k := o.Key()
		if desiredKeys[k] || seen[k] {
			continue
		}
		seen[k] = true
		candidates = append(candidates, o)
	}
	if len(candidates) == 0 {
		return planned, nil
	}
	r, ok := d.(driver.ObjectReader)
	if !ok {
		return planned, nil
	}
	lives, err := r.Read(ctx, owner, candidates)
	if err != nil {
		return nil, err
	}
	liveKeys := make(map[string]bool, len(lives))
	for _, l := range lives {
		liveKeys[l.Object.Key()] = true
	}
	out := make([]driver.Diff, 0, len(planned)+len(lives))
	for _, df := range planned {
		if df.Op == driver.OpDelete && liveKeys[df.Object.Key()] {
			continue
		}
		out = append(out, df)
	}
	for _, l := range lives {
		out = append(out, driver.Diff{Object: l.Object, Op: driver.OpDelete})
	}
	return out, nil
}
