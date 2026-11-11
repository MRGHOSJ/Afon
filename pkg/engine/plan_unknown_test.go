package engine

import (
	"context"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver/fake"
)

// TestPlan_UnknownActionUpstream: a will-run action upstream of another
// action means the downstream key cannot be computed at plan time — the
// plan predicts will-run with no key, an upper bound apply may beat by
// hitting the cache (engine rule 1).
func TestPlan_UnknownActionUpstream(t *testing.T) {
	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: chain}
spec:
  nodes:
    - id: first
      type: image-ref
      config: { ref: nginx:1.27 }
    - id: second
      type: image-ref
      config: { ref: "${first.ref}-slim" }
`
	cf, err := compile(t, src)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	d := fake.New()

	p, err := BuildPlan(context.Background(), cf, store, d)
	if err != nil {
		t.Fatal(err)
	}
	first, second := p.Entries[0], p.Entries[1]
	if first.Status != PlanWillRun {
		t.Errorf("first = %+v", first)
	}
	if second.Status != PlanWillRun {
		t.Errorf("second = %+v", second)
	}
	if second.Key != "" {
		t.Errorf("unknown upstream must block key computation, got %q", second.Key)
	}
	if second.Reason == "" {
		t.Error("second must say why it will run")
	}

	// Apply: both run, keys computed with real values.
	res, err := Apply(context.Background(), cf, store, d, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res.Get("first"); r.Cached || r.Status != StatusSucceeded {
		t.Errorf("first = %+v", r)
	}
	if r, _ := res.Get("second"); r.Cached || r.Status != StatusSucceeded {
		t.Errorf("second = %+v", r)
	}

	// Second plan: both cached, second now has a key.
	p2, err := BuildPlan(context.Background(), cf, store, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p2.Entries {
		if e.Status != PlanCached {
			t.Errorf("entry %s = %s after apply, want cached", e.NodeID, e.Status)
		}
		if e.Key == "" {
			t.Errorf("entry %s must have a key when inputs are known", e.NodeID)
		}
	}
}

// TestPlan_WillRunIsUpperBound (rule 1): a plan that says will-run can
// still produce a cached apply — the key is computed at apply time and
// may hit then. Construct: plan against an empty store, fill the store
// behind the plan's back, apply, expect Cached.
func TestPlan_WillRunIsUpperBound(t *testing.T) {
	cf, err := compile(t, minimalFlow)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	d := fake.New()
	ctx := context.Background()

	p, err := BuildPlan(ctx, cf, store, d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[0].Status != PlanWillRun {
		t.Fatalf("entry = %+v", p.Entries[0])
	}

	// Warm the cache after the plan was built (another process could have
	// applied the same flow in between).
	if _, err := Apply(ctx, cf, store, d, ExecutorOptions{}); err != nil {
		t.Fatal(err)
	}
	// Re-build the plan now and it says cached; but even the STALE plan's
	// prediction is only an upper bound: apply reports the truth.
	res, err := Apply(ctx, cf, store, d, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res.Get("image"); !r.Cached {
		t.Errorf("apply must report cached, plan said will-run (upper bound): %+v", r)
	}
}
