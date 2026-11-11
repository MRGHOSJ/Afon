package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver/fake"
)

const twoNodeFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: image
      type: image-ref
      config: { ref: nginx:1.27 }
    - id: obj
      type: fake-object
      config:
        kind: ConfigMap
        name: hello
        data:
          digest: ${image.digest}
`

// TestPlan_WillRunThenKnownAfterApply covers the "will-run action upstream
// of a resource" path: the action's outputs are unknown, so the resource
// cannot be diffed and must say known-after-apply.
func TestPlan_WillRunThenKnownAfterApply(t *testing.T) {
	cf, err := compile(t, twoNodeFlow)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	d := fake.New()

	p, err := BuildPlan(context.Background(), cf, store, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 2 {
		t.Fatalf("entries = %d", len(p.Entries))
	}
	img, obj := p.Entries[0], p.Entries[1]
	if img.NodeID != "image" || img.Status != PlanWillRun {
		t.Errorf("image entry = %+v", img)
	}
	// image's own config has no references, so its key IS computable even
	// though it will run; what matters is that its outputs are unknown to
	// downstream planning.
	if obj.NodeID != "obj" || obj.Status != PlanKnownAfterApply {
		t.Errorf("obj entry = %+v", obj)
	}
	if obj.Reason == "" || obj.Diff != nil {
		t.Errorf("known-after-apply must explain and carry no diff: %+v", obj)
	}
	if p.Summary[PlanWillRun] != 1 || p.Summary[PlanKnownAfterApply] != 1 {
		t.Errorf("summary = %v", p.Summary)
	}
}

// TestPlan_CachedActionUpstream covers the other path: a cache hit supplies
// real outputs, so the downstream resource gets a real diff at plan time.
func TestPlan_CachedActionUpstream(t *testing.T) {
	cf, err := compile(t, twoNodeFlow)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	d := fake.New()
	ctx := context.Background()

	// First apply puts the action's outputs in the cache.
	if _, err := Apply(ctx, cf, store, d, ExecutorOptions{}); err != nil {
		t.Fatal(err)
	}

	p, err := BuildPlan(ctx, cf, store, d)
	if err != nil {
		t.Fatal(err)
	}
	img, obj := p.Entries[0], p.Entries[1]
	if img.Status != PlanCached || img.Key == "" {
		t.Errorf("image entry = %+v", img)
	}
	// The object was applied, so the plan now says none — not
	// known-after-apply: the cached outputs were real.
	if obj.Status != PlanNone {
		t.Errorf("obj entry = %+v, want none (real diff from cached outputs)", obj)
	}
	if obj.Diff == nil {
		t.Error("obj entry must carry the computed diff")
	}
}

// TestPlan_ReadOnly asserts plan never mutates the driver or the cache.
func TestPlan_ReadOnly(t *testing.T) {
	cf, err := compile(t, twoNodeFlow)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	d := fake.New()

	if _, err := BuildPlan(context.Background(), cf, store, d); err != nil {
		t.Fatal(err)
	}
	if len(d.Objects()) != 0 {
		t.Errorf("plan mutated driver: %v", d.Objects())
	}
	if _, ok, _ := store.Get("anything"); ok {
		t.Error("plan wrote the cache")
	}
	// And again after an apply: a second plan must not change state.
	if _, err := Apply(context.Background(), cf, store, d, ExecutorOptions{}); err != nil {
		t.Fatal(err)
	}
	before := len(d.Objects())
	keysBefore := len(store.entries)
	for i := 0; i < 3; i++ {
		if _, err := BuildPlan(context.Background(), cf, store, d); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.Objects()) != before {
		t.Error("repeated plan mutated driver")
	}
	if len(store.entries) != keysBefore {
		t.Error("repeated plan wrote the cache")
	}
}

// TestApplyTwiceThenPlan is the cross-run contract: apply, apply, plan →
// everything cached or none (the no-op rerun demo). Uses the disk-backed
// store and persistent fake driver, as the CLI does.
func TestApplyTwiceThenPlan(t *testing.T) {
	dir := t.TempDir()
	flowPath := filepath.Join(dir, "flow.yaml")
	// The flow file's location defines the state dir for the CLI; here we
	// point the store and driver at the same place manually.
	_ = flowPath

	cf, err := compile(t, twoNodeFlow)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDiskStore(filepath.Join(dir, ".afon", "cache"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := fake.NewPersistent(filepath.Join(dir, ".afon", "fake-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Apply 1: everything runs.
	res1, err := Apply(ctx, cf, store, d, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res1.Get("image"); r.Cached {
		t.Error("first apply must not be cached")
	}
	if r, _ := res1.Get("obj"); r.DiffCount != 1 {
		t.Errorf("first apply obj diffCount = %d, want 1", r.DiffCount)
	}

	// Apply 2: action cached (same store on disk), resource no diffs.
	// Simulate a fresh process: new driver instance reading the same file.
	d2, err := fake.NewPersistent(filepath.Join(dir, ".afon", "fake-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	res2, err := Apply(ctx, cf, store, d2, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res2.Get("image"); !r.Cached {
		t.Error("second apply must hit the cache")
	}
	if r, _ := res2.Get("obj"); r.DiffCount != 0 {
		t.Errorf("second apply obj diffCount = %d, want 0", r.DiffCount)
	}

	// Plan after two applies: everything cached or none.
	p, err := BuildPlan(ctx, cf, store, d2)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Entries {
		switch e.Status {
		case PlanCached, PlanNone:
		default:
			t.Errorf("entry %s = %s, want cached or none", e.NodeID, e.Status)
		}
	}
}
