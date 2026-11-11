package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/driver/fake"
)

// refChangeFlow builds the two-node image→object flow at a given ref, so
// a test can swap the ref between runs to force a downstream update.
func refChangeFlow(ref string) string {
	return `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: journey}
spec:
  nodes:
    - id: image
      type: image-ref
      config: { ref: ` + ref + ` }
    - id: obj
      type: fake-object
      config:
        kind: ConfigMap
        name: hello
        data:
          digest: ${image.digest}
`
}

// TestPlan_UpdateAfterRefChange is the full journey for an upstream ref
// bump: apply v1 (create), plan v2 (will-run + known-after-apply — the
// new digest does not exist yet), apply v2 (the object updates), plan
// again (cached + none).
func TestPlan_UpdateAfterRefChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewDiskStore(filepath.Join(dir, ".afon", "cache"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := fake.NewPersistent(filepath.Join(dir, ".afon", "fake-state.json"))
	if err != nil {
		t.Fatal(err)
	}

	v1, err := compile(t, refChangeFlow("nginx:1.27"))
	if err != nil {
		t.Fatal(err)
	}
	// Apply v1: object created.
	res1, err := Apply(ctx, v1, store, d, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res1.Get("obj"); r.DiffCount != 1 || r.Detail != "created ConfigMap/hello" {
		t.Errorf("v1 obj = %+v, want diffCount 1 and created detail", r)
	}

	// Plan v2: the ref changed, so the image will run and its outputs are
	// unknown — the object cannot be diffed yet (rule 2).
	v2, err := compile(t, refChangeFlow("nginx:1.28"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(ctx, v2, store, d)
	if err != nil {
		t.Fatal(err)
	}
	img, obj := p.Entries[0], p.Entries[1]
	if img.NodeID != "image" || img.Status != PlanWillRun {
		t.Errorf("image entry = %+v, want will-run", img)
	}
	if obj.NodeID != "obj" || obj.Status != PlanKnownAfterApply {
		t.Errorf("obj entry = %+v, want known-after-apply", obj)
	}

	// Apply v2: image runs with the new ref, object updates in place.
	res2, err := Apply(ctx, v2, store, d, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res2.Get("image"); r.Cached {
		t.Error("changed ref must not hit the cache")
	}
	if r, _ := res2.Get("obj"); r.DiffCount != 1 || r.Detail != "updated ConfigMap/hello" {
		t.Errorf("v2 obj = %+v, want diffCount 1 and updated detail", r)
	}

	// Plan again: everything settled — cached + none.
	p2, err := BuildPlan(ctx, v2, store, d)
	if err != nil {
		t.Fatal(err)
	}
	if e := p2.Entries[0]; e.Status != PlanCached {
		t.Errorf("image entry = %+v, want cached", e)
	}
	if e := p2.Entries[1]; e.Status != PlanNone {
		t.Errorf("obj entry = %+v, want none", e)
	}
}

// dataChangeFlow builds image→object with an extra literal data field the
// test can change between runs without touching the image ref.
func dataChangeFlow(env string) string {
	return `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: datachange}
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
          env: ` + env + `
`
}

// TestPlan_DataOnlyChange: the image key is warm (cache hit supplies the
// digest), but the object's own data changed — the plan must say cached
// for the action and update for the resource, and apply must report the
// update detail.
func TestPlan_DataOnlyChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewDiskStore(filepath.Join(dir, ".afon", "cache"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := fake.NewPersistent(filepath.Join(dir, ".afon", "fake-state.json"))
	if err != nil {
		t.Fatal(err)
	}

	v1, err := compile(t, dataChangeFlow("prod"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, v1, store, d, ExecutorOptions{}); err != nil {
		t.Fatal(err)
	}

	// Only the literal env field changes; the ref (and therefore the
	// image key) stays identical.
	v2, err := compile(t, dataChangeFlow("staging"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(ctx, v2, store, d)
	if err != nil {
		t.Fatal(err)
	}
	img, obj := p.Entries[0], p.Entries[1]
	if img.Status != PlanCached || img.Key == "" {
		t.Errorf("image entry = %+v, want cached with a key", img)
	}
	if obj.Status != PlanUpdate {
		t.Errorf("obj entry = %+v, want update", obj)
	}
	if obj.Diff == nil {
		t.Fatal("obj entry must carry the computed diff")
	}
	want, err := driver.Canonical(obj.Diff.Object.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(want), "staging") {
		t.Errorf("obj diff data = %s, want desired data containing staging", want)
	}

	// Apply: the image short-circuits on the cache; only the object runs.
	res, err := Apply(ctx, v2, store, d, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res.Get("image"); !r.Cached {
		t.Error("unchanged ref must hit the cache")
	}
	if r, _ := res.Get("obj"); r.DiffCount != 1 || r.Detail != "updated ConfigMap/hello" {
		t.Errorf("obj = %+v, want diffCount 1 and updated detail", r)
	}
}
