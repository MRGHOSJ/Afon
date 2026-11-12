package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

// recordDriver captures the Owner every Plan/Apply call carries and
// answers Plan with create diffs so the apply path runs.
type recordDriver struct {
	mu          sync.Mutex
	planOwners  []driver.Owner
	applyOwners []driver.Owner
}

func (r *recordDriver) Plan(ctx context.Context, owner driver.Owner, desired []driver.Object) ([]driver.Diff, error) {
	r.mu.Lock()
	r.planOwners = append(r.planOwners, owner)
	r.mu.Unlock()
	diffs := make([]driver.Diff, 0, len(desired))
	for _, o := range desired {
		diffs = append(diffs, driver.Diff{Object: o, Op: driver.OpCreate})
	}
	return diffs, nil
}

func (r *recordDriver) Apply(ctx context.Context, owner driver.Owner, diffs []driver.Diff) error {
	r.mu.Lock()
	r.applyOwners = append(r.applyOwners, owner)
	r.mu.Unlock()
	return nil
}

// TestOwner_Plumbing: plan and apply stamp driver calls with
// Owner{Flow: flow name, Node: resource node id} — the owner the Phase 3
// inventory driver will label and prune by.
func TestOwner_Plumbing(t *testing.T) {
	cf, err := compile(t, twoNodeFlow)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store := NewMemoryStore()
	d := &recordDriver{}
	want := driver.Owner{Flow: "t", Node: "obj"}

	// Cold plan: the action's outputs are unknown, so the resource
	// degrades to known-after-apply and never reaches the driver.
	if _, err := BuildPlan(ctx, cf, store, d); err != nil {
		t.Fatal(err)
	}
	if len(d.planOwners) != 0 {
		t.Fatalf("cold plan reached the driver: %+v", d.planOwners)
	}

	// Apply: the in-run plan and the apply both carry the owner.
	if _, err := Apply(ctx, cf, store, d, ExecutorOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(d.applyOwners) != 1 {
		t.Fatalf("apply owners = %+v, want exactly the resource node", d.applyOwners)
	}
	if d.applyOwners[0] != want {
		t.Errorf("apply owner = %+v, want %+v", d.applyOwners[0], want)
	}
	if len(d.planOwners) != 1 || d.planOwners[0] != want {
		t.Errorf("in-run plan owners = %+v, want [%+v]", d.planOwners, want)
	}

	// Warm plan (action cached → resource diffs at plan time): same owner.
	if _, err := BuildPlan(ctx, cf, store, d); err != nil {
		t.Fatal(err)
	}
	if len(d.planOwners) != 2 {
		t.Fatalf("warm plan owners = %+v, want the resource node again", d.planOwners)
	}
	if d.planOwners[1] != want {
		t.Errorf("warm plan owner = %+v, want %+v", d.planOwners[1], want)
	}
}
