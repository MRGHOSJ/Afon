package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver/fake"
)

const matPlanFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: m
      type: test-mat
      config:
        value: hello
        out: gen/out.yaml
    - id: obj
      type: fake-object
      config:
        kind: ConfigMap
        name: demo
        data:
          msg: ${m.rendered}
`

// TestPlan_MaterializedDownstreamDiff is the headline: with the file
// rendered, the downstream resource plans a real create — not
// known-after-apply — and the action entry says file.
func TestPlan_MaterializedDownstreamDiff(t *testing.T) {
	cf := compileMat(t, matPlanFlow)
	ctx := context.Background()
	store := NewMemoryStore()
	d := fake.New()

	// Before render: plan fails loudly, naming the fix.
	_, err := BuildPlan(ctx, cf, store, d)
	if err == nil || !strings.Contains(err.Error(), `node "m"`) ||
		!strings.Contains(err.Error(), "gen/out.yaml") ||
		!strings.Contains(err.Error(), `run "afon render" first`) {
		t.Fatalf("missing-file plan err = %v", err)
	}

	if _, err := RenderActions(ctx, cf, RenderOptions{}); err != nil {
		t.Fatal(err)
	}

	p, err := BuildPlan(ctx, cf, store, d)
	if err != nil {
		t.Fatal(err)
	}
	m, obj := p.Entries[0], p.Entries[1]
	if m.Status != PlanFile || m.Reason == "" {
		t.Errorf("m entry = %+v, want file status with path reason", m)
	}
	if m.Key != "" {
		t.Errorf("materialized entry must not consult the cache: %+v", m)
	}
	if obj.Status != PlanCreate || obj.Diff == nil {
		t.Errorf("obj entry = %+v, want a real create diff", obj)
	}
	if p.Summary[PlanFile] != 1 {
		t.Errorf("summary = %v", p.Summary)
	}
	outs, ok := p.ActionOutputs["m"]
	if !ok || outs["rendered"].Data != "hello\n" {
		t.Errorf("ActionOutputs = %+v", p.ActionOutputs)
	}
	if !strings.Contains(p.RenderText(), "file") {
		t.Errorf("RenderText missing file status:\n%s", p.RenderText())
	}

	// Apply executes: resource node gets a real diff, not degradation.
	res, err := Apply(ctx, cf, store, d, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mr, ok := res.Get("m")
	if !ok || mr.Status != StatusSucceeded || mr.Cached {
		t.Errorf("m result = %+v", mr)
	}
	if !strings.Contains(mr.Detail, "from ") {
		t.Errorf("m detail = %q, want from <path>", mr.Detail)
	}
	if got := mr.Outputs["rendered"]; got.Data != "hello\n" {
		t.Errorf("m outputs = %+v, want file body", got)
	}
	or, _ := res.Get("obj")
	if or.Status != StatusSucceeded || or.DiffCount != 1 {
		t.Errorf("obj result = %+v", or)
	}

	// Converged: plan now says none.
	p, err = BuildPlan(ctx, cf, store, d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[1].Status != PlanNone {
		t.Errorf("warm obj entry = %+v, want none", p.Entries[1])
	}
}

// TestPlan_MaterializedMissingFileWithoutRefs pins the outputs-override
// rationale: even with nothing referencing the node, a missing file
// fails plan — no silent fallback to cache or will-run.
func TestPlan_MaterializedMissingFileWithoutRefs(t *testing.T) {
	cf := compileMat(t, matFlow) // action only, no downstream ref
	_, err := BuildPlan(context.Background(), cf, NewMemoryStore(), fake.New())
	if err == nil || !strings.Contains(err.Error(), "missing at") ||
		!strings.Contains(err.Error(), `run "afon render" first`) {
		t.Fatalf("err = %v, want missing-file error", err)
	}
}

// TestPlan_EphemeralUnchanged: without out:, nothing materializes and
// the old known-after-apply behavior is untouched.
func TestPlan_EphemeralUnchanged(t *testing.T) {
	flow := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: m
      type: test-mat
      config: { value: hello }
    - id: obj
      type: fake-object
      config:
        kind: ConfigMap
        name: demo
        data:
          msg: ${m.rendered}
`
	cf := compileMat(t, flow)
	p, err := BuildPlan(context.Background(), cf, NewMemoryStore(), fake.New())
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[0].Status != PlanWillRun {
		t.Errorf("ephemeral action entry = %+v, want will-run", p.Entries[0])
	}
	if p.Entries[1].Status != PlanKnownAfterApply {
		t.Errorf("downstream entry = %+v, want known-after-apply", p.Entries[1])
	}
}

// TestApply_MaterializedBypassesCache: even with garbage pre-seeded
// under the node's own cache key, apply serves the file — the file is
// the truth, cache plays no part for materialized nodes.
func TestApply_MaterializedBypassesCache(t *testing.T) {
	cf := compileMat(t, matFlow)
	ctx := context.Background()
	if _, err := RenderActions(ctx, cf, RenderOptions{}); err != nil {
		t.Fatal(err)
	}

	store := NewMemoryStore()
	cn, _ := cf.Node("m")
	resolved, err := Resolve(cn.Config, func(string, string) (Value, bool) { return Value{}, false })
	if err != nil || !resolved.Known {
		t.Fatalf("resolve: %v/%v", resolved, err)
	}
	key, err := actionCacheKey(cn, resolved, Inputs{FlowFile: cf.File, Target: cf.Target})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(key, Outputs{"rendered": KnownValue("CACHED-TRASH\n")}); err != nil {
		t.Fatal(err)
	}

	res, err := Apply(ctx, cf, store, nilDriver(), ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mr, _ := res.Get("m")
	if mr.Cached {
		t.Error("materialized node must not report cached")
	}
	if got := mr.Outputs["rendered"]; got.Data != "hello\n" {
		t.Errorf("outputs = %+v, want file body, not cache", got)
	}
}

// TestStaleMaterialized: config drift after render warns; clean state,
// missing files, and headerless files do not.
func TestStaleMaterialized(t *testing.T) {
	cf := compileMat(t, matPlanFlow)
	ctx := context.Background()

	// No file yet: skipped (missing is plan's hard error, not a warning).
	warns, err := StaleMaterialized(cf)
	if err != nil || len(warns) != 0 {
		t.Fatalf("pre-render warns = %v, err = %v", warns, err)
	}

	if _, err := RenderActions(ctx, cf, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	warns, err = StaleMaterialized(cf)
	if err != nil || len(warns) != 0 {
		t.Fatalf("fresh render warns = %v, err = %v", warns, err)
	}

	// Flow config changed, file untouched: warn, naming node and path.
	editedFlow := strings.Replace(matPlanFlow, "value: hello", "value: hello2", 1)
	staleCF := compileMat(t, editedFlow)
	staleCF.File = cf.File
	warns, err = StaleMaterialized(staleCF)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 || warns[0].NodeID != "m" || !strings.Contains(warns[0].Path, "out.yaml") {
		t.Fatalf("stale warns = %+v", warns)
	}

	// Headerless file: skipped, not a warning.
	path := filepath.FromSlash(warns[0].Path)
	if err := os.WriteFile(path, []byte("value: hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warns, err = StaleMaterialized(staleCF)
	if err != nil || len(warns) != 0 {
		t.Fatalf("headerless warns = %v, err = %v", warns, err)
	}
}

// TestMaterializedOutputs_HeaderStripped: plan/apply see the manifest
// body, never the provenance header.
func TestMaterializedOutputs_HeaderStripped(t *testing.T) {
	cf := compileMat(t, matFlow)
	ctx := context.Background()
	if _, err := RenderActions(ctx, cf, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	cn, _ := cf.Node("m")
	outs, path, ok, err := materializedOutputs(cn, Inputs{FlowFile: cf.File, Target: cf.Target})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if path == "" || strings.Contains(path, "\\") {
		t.Errorf("path = %q, want slash-normalized", path)
	}
	got := outs["rendered"]
	if !got.Known || got.Data != "hello\n" {
		t.Errorf("output = %+v, want body without header", got)
	}
	if s, _ := got.Data.(string); strings.Contains(s, "# Generated by afon render") {
		t.Error("header leaked into outputs")
	}
}
