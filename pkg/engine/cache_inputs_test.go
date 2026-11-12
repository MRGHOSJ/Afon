package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/schema"
)

// cacherNode is an action node that implements InputCacher: its
// (simulated) output depends on the flow target, so the target must be
// part of its cache key.
type cacherNode struct {
	testNodeType
	cacheInputs func(cfg any, in Inputs) (string, error)
}

func (c *cacherNode) CacheInputs(cfg any, in Inputs) (string, error) {
	return c.cacheInputs(cfg, in)
}

// newCacherNode builds a cacher whose extra is "ns=<target>".
func newCacherNode(t *testing.T) *cacherNode {
	t.Helper()
	return &cacherNode{
		testNodeType: testNodeType{name: "cacher", kind: KindAction, outputs: []string{"v"},
			run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
				return Outputs{"v": KnownValue("ok")}, nil
			}},
		cacheInputs: func(cfg any, in Inputs) (string, error) {
			return "ns=" + in.Target.Namespace, nil
		},
	}
}

// regFor registers nt in a fresh registry (compileWith needs it).
func regFor(t *testing.T, nt NodeType) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register(nt); err != nil {
		t.Fatal(err)
	}
	return reg
}

const cacherFlowTpl = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  target: {namespace: %s}
  nodes:
    - id: a
      type: cacher
      config: {}
`

// TestCacheInputs_TargetChangeInvalidates is the fourth non-negotiable
// cache test (the file add/edit/delete three land with k8s-manifests in
// Stage 3): same config, same files, different target.namespace →
// different key → cache miss → re-run with the new namespace. Without
// this, staging-rendered output would be reused under production.
func TestCacheInputs_TargetChangeInvalidates(t *testing.T) {
	nt := newCacherNode(t)
	reg := regFor(t, nt)
	ctx := context.Background()
	store := NewMemoryStore()

	cfStaging, err := compileWith(t, fmt.Sprintf(cacherFlowTpl, "staging"), reg)
	if err != nil {
		t.Fatal(err)
	}
	// Cold plan: key computable, nothing cached.
	p1, err := BuildPlan(ctx, cfStaging, store, nilDriver())
	if err != nil {
		t.Fatal(err)
	}
	if p1.Entries[0].Status != PlanWillRun || p1.Entries[0].Key == "" {
		t.Fatalf("staging plan = %+v", p1.Entries[0])
	}
	keyStaging := p1.Entries[0].Key

	// Apply staging: writes the entry, runs once.
	if _, err := Apply(ctx, cfStaging, store, nilDriver(), ExecutorOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := nt.runCount.Load(); n != 1 {
		t.Fatalf("runs after staging apply = %d, want 1", n)
	}
	// Warm staging plan: cached.
	pWarm, err := BuildPlan(ctx, cfStaging, store, nilDriver())
	if err != nil {
		t.Fatal(err)
	}
	if pWarm.Entries[0].Status != PlanCached {
		t.Fatalf("staging warm plan = %q, want cached", pWarm.Entries[0].Status)
	}

	// Same everything except target.namespace: production must MISS.
	cfProd, err := compileWith(t, fmt.Sprintf(cacherFlowTpl, "production"), reg)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := BuildPlan(ctx, cfProd, store, nilDriver())
	if err != nil {
		t.Fatal(err)
	}
	if p2.Entries[0].Status == PlanCached {
		t.Fatal("target change must invalidate the cache: plan says cached")
	}
	if p2.Entries[0].Key == keyStaging {
		t.Fatalf("key did not change with target: %s", p2.Entries[0].Key)
	}

	// Apply production re-runs (the stale staging output is not reused).
	res, err := Apply(ctx, cfProd, store, nilDriver(), ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res.Get("a"); r.Cached {
		t.Error("production apply must not hit the staging entry")
	}
	if n := nt.runCount.Load(); n != 2 {
		t.Errorf("runs = %d, want 2 (staging + production)", n)
	}
}

// TestActionCacheKey_NoChurnForPlainActions: nodes that do not implement
// InputCacher keep the exact CacheKey payload — no existing keys churn —
// and the cacher's key is deterministic across calls (canonical
// {config, extra} serialization, no map iteration).
func TestActionCacheKey_NoChurnForPlainActions(t *testing.T) {
	cf, err := compile(t, twoNodeFlow)
	if err != nil {
		t.Fatal(err)
	}
	cn, ok := cf.Node("image")
	if !ok {
		t.Fatal("image node missing")
	}
	resolved, err := Resolve(cn.Config, func(node, output string) (Value, bool) {
		return Value{}, false
	})
	if err != nil {
		t.Fatal(err)
	}
	in := Inputs{FlowFile: "flow.yaml", Target: schema.Target{Namespace: "anywhere"}}
	got, err := actionCacheKey(cn, resolved, in)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CacheKey(cn.Type, cn.NT.TypeVersion(), resolved.Value)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("non-cacher key churned: %s vs %s", got, want)
	}

	// Cacher determinism: ten computations, one key.
	nt := newCacherNode(t)
	reg := regFor(t, nt)
	cfC, err := compileWith(t, fmt.Sprintf(cacherFlowTpl, "staging"), reg)
	if err != nil {
		t.Fatal(err)
	}
	cnC, _ := cfC.Node("a")
	resC, err := Resolve(cnC.Config, func(node, output string) (Value, bool) {
		return Value{}, false
	})
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for i := 0; i < 10; i++ {
		k, err := actionCacheKey(cnC, resC, in)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = k
		} else if k != first {
			t.Fatalf("key drifted across calls: %s vs %s", first, k)
		}
	}
	// And the Inputs actually reach CacheInputs: namespace participates.
	other := Inputs{FlowFile: "flow.yaml", Target: schema.Target{Namespace: "elsewhere"}}
	k1, _ := actionCacheKey(cnC, resC, in)
	k2, _ := actionCacheKey(cnC, resC, other)
	if k1 == k2 {
		t.Error("CacheInputs contribution missing from key")
	}
}

// TestCacheInputs_ErrorAsymmetry: InputCacher failures are asymmetric by
// design — plan degrades to will-run with the reason (plan never
// executes), apply fails the node.
func TestCacheInputs_ErrorAsymmetry(t *testing.T) {
	nt := &cacherNode{
		testNodeType: testNodeType{name: "cacher", kind: KindAction, outputs: []string{"v"}},
		cacheInputs: func(cfg any, in Inputs) (string, error) {
			return "", fmt.Errorf("manifest files unreadable")
		},
	}
	reg := regFor(t, nt)
	src := fmt.Sprintf(cacherFlowTpl, "staging")
	cf, err := compileWith(t, src, reg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Plan: will-run with the reason, not an error.
	p, err := BuildPlan(ctx, cf, NewMemoryStore(), nilDriver())
	if err != nil {
		t.Fatal(err)
	}
	e := p.Entries[0]
	if e.Status != PlanWillRun {
		t.Errorf("status = %q, want will-run", e.Status)
	}
	if !strings.Contains(e.Reason, "cache inputs") {
		t.Errorf("reason = %q, want it to mention cache inputs", e.Reason)
	}

	// Apply: the node fails; the error names the cache-input failure.
	res, err := Apply(ctx, cf, NewMemoryStore(), nilDriver(), ExecutorOptions{})
	if err == nil {
		t.Fatal("apply must fail when CacheInputs fails")
	}
	if !strings.Contains(err.Error(), "cache inputs") {
		t.Errorf("apply error = %v, want it to mention cache inputs", err)
	}
	if r, _ := res.Get("a"); r.Status != StatusFailed {
		t.Errorf("status = %q, want failed", r.Status)
	}
}
