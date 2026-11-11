package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/schema"
)

// testNodeType is a configurable fake node type for executor tests.
type testNodeType struct {
	name     string
	kind     Kind
	outputs  []string
	validate func(cfg any) error
	run      func(ctx context.Context, cfg any, in Inputs) (Outputs, error)
	desired  func(ctx context.Context, cfg any, in Inputs) ([]driver.Object, error)
	// runCount counts Run calls for cache assertions.
	runCount atomic.Int32
}

func (t *testNodeType) Type() string              { return t.name }
func (t *testNodeType) TypeVersion() string       { return "1" }
func (t *testNodeType) Kind() Kind                { return t.kind }
func (t *testNodeType) DeclaredOutputs() []string { return t.outputs }

func (t *testNodeType) Validate(cfg any) error {
	if t.validate != nil {
		return t.validate(cfg)
	}
	return nil
}

func (t *testNodeType) Run(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
	t.runCount.Add(1)
	if t.run != nil {
		return t.run(ctx, cfg, in)
	}
	return Outputs{}, nil
}

func (t *testNodeType) Desired(ctx context.Context, cfg any, in Inputs) ([]driver.Object, error) {
	if t.desired != nil {
		return t.desired(ctx, cfg, in)
	}
	return nil, nil
}

// regWith builds a registry from ad-hoc test types.
func regWith(t *testing.T, types ...*testNodeType) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, nt := range types {
		if err := reg.Register(nt); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// TestExecutor_Diamond runs a diamond graph (a→b, a→c, b→d, c→d) under
// -race and checks that d sees both branches' outputs and every node
// succeeds exactly once.
func TestExecutor_Diamond(t *testing.T) {
	a := &testNodeType{name: "gen", kind: KindAction, outputs: []string{"v"},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			return Outputs{"v": KnownValue("seed")}, nil
		}}
	b := &testNodeType{name: "left", kind: KindAction, outputs: []string{"v"},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			return Outputs{"v": KnownValue("left")}, nil
		}}
	c := &testNodeType{name: "right", kind: KindAction, outputs: []string{"v"},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			return Outputs{"v": KnownValue("right")}, nil
		}}
	d := &testNodeType{name: "join", kind: KindAction, outputs: []string{"out"},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			m, _ := cfg.(map[string]any)
			l, _ := m["l"].(string)
			r, _ := m["r"].(string)
			if l != "left" || r != "right" {
				return nil, fmt.Errorf("got l=%q r=%q", l, r)
			}
			return Outputs{"out": KnownValue(l + "+" + r)}, nil
		}}

	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: diamond}
spec:
  nodes:
    - id: a
      type: gen
      config: {}
    - id: b
      type: left
      config: {v: "${a.v}"}
    - id: c
      type: right
      config: {v: "${a.v}"}
    - id: d
      type: join
      config:
        l: ${b.v}
        r: ${c.v}
`
	flow, err := compileWith(t, src, regWith(t, a, b, c, d))
	if err != nil {
		t.Fatal(err)
	}
	// Levels prove the diamond shape: b and c share a level.
	if flow.Levels["b"] != flow.Levels["c"] {
		t.Errorf("b and c must be siblings: %v", flow.Levels)
	}
	if flow.Levels["d"] != flow.Levels["b"]+1 {
		t.Errorf("d must follow b/c: %v", flow.Levels)
	}

	res, err := Apply(context.Background(), flow, NewMemoryStore(), nilDriver(), ExecutorOptions{Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	snap := res.Snapshot()
	for _, id := range []string{"a", "b", "c", "d"} {
		r, ok := snap[id]
		if !ok || r.Status != StatusSucceeded {
			t.Errorf("node %s = %+v, want succeeded", id, r)
		}
	}
	if r := snap["d"]; r.Outputs["out"].Data != "left+right" {
		t.Errorf("join output = %v", r.Outputs["out"].Data)
	}
	// Each generator ran exactly once.
	for _, nt := range []*testNodeType{a, b, c, d} {
		if n := nt.runCount.Load(); n != 1 {
			t.Errorf("%s ran %d times, want 1", nt.name, n)
		}
	}
}

// TestExecutor_FailStopsScheduling: a failing node halts the run; a slow
// in-flight sibling is cancelled (not failed); unscheduled nodes are
// skipped. Run under -race.
func TestExecutor_FailStopsScheduling(t *testing.T) {
	fail := &testNodeType{name: "boom", kind: KindAction, outputs: []string{},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			return nil, errors.New("kaboom")
		}}
	slow := &testNodeType{name: "slow", kind: KindAction, outputs: []string{"v"},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			select {
			case <-time.After(5 * time.Second):
				return Outputs{"v": KnownValue("late")}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
	never := &testNodeType{name: "never", kind: KindAction, outputs: []string{},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			t.Error("never must not run")
			return Outputs{}, nil
		}}

	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: fail}
spec:
  nodes:
    - id: slow
      type: slow
      config: {}
    - id: boom
      type: boom
      config: {}
    - id: tail
      type: never
      needs: [boom]
      config: {}
`
	flow, err := compileWith(t, src, regWith(t, fail, slow, never))
	if err != nil {
		t.Fatal(err)
	}

	res, err := Apply(context.Background(), flow, NewMemoryStore(), nilDriver(), ExecutorOptions{Parallelism: 4})
	if err == nil {
		t.Fatal("expected an error from the failed run")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("error should surface kaboom: %v", err)
	}
	snap := res.Snapshot()
	if r := snap["boom"]; r.Status != StatusFailed {
		t.Errorf("boom = %s, want failed", r.Status)
	}
	// The slow sibling was in flight when boom failed: it must be
	// cancelled, not failed, and definitely not succeeded.
	if r := snap["slow"]; r.Status != StatusCancelled {
		t.Errorf("slow = %s, want cancelled", r.Status)
	}
	// tail depends on boom and was never scheduled: skipped.
	if r := snap["tail"]; r.Status != StatusSkipped {
		t.Errorf("tail = %s, want skipped", r.Status)
	}
	if !res.Failed {
		t.Error("RunResult.Failed must be true")
	}
}

// TestExecutor_CancelledNotFailed: cancelling the caller's context marks
// everything cancelled, never failed.
func TestExecutor_CancelledNotFailed(t *testing.T) {
	block := &testNodeType{name: "block", kind: KindAction, outputs: []string{},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: cancel}
spec:
  nodes:
    - id: a
      type: block
      config: {}
    - id: b
      type: block
      needs: [a]
      config: {}
`
	flow, err := compileWith(t, src, regWith(t, block))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	res, err := Apply(ctx, flow, NewMemoryStore(), nilDriver(), ExecutorOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	for id, r := range res.Snapshot() {
		if r.Status == StatusFailed {
			t.Errorf("node %s failed on cancellation: %+v", id, r)
		}
	}
}

// TestExecutor_NoCacheWriteOnFailure (rule 4): a failed action must leave
// no cache entry, so the next run tries again.
func TestExecutor_NoCacheWriteOnFailure(t *testing.T) {
	attempt := atomic.Int32{}
	flaky := &testNodeType{name: "flaky", kind: KindAction, outputs: []string{"v"},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			if attempt.Add(1) == 1 {
				return nil, errors.New("transient")
			}
			return Outputs{"v": KnownValue("ok")}, nil
		}}
	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: cache}
spec:
  nodes:
    - id: a
      type: flaky
      config: {}
`
	flow, err := compileWith(t, src, regWith(t, flaky))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()

	if _, err := Apply(context.Background(), flow, store, nilDriver(), ExecutorOptions{}); err == nil {
		t.Fatal("first run must fail")
	}
	if len(store.entries) != 0 {
		t.Fatalf("failed run wrote cache entries: %v", SortedKeys(store.entries))
	}

	// Second run succeeds and writes the entry.
	if _, err := Apply(context.Background(), flow, store, nilDriver(), ExecutorOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 1 {
		t.Fatalf("successful run must write exactly one entry: %v", SortedKeys(store.entries))
	}
}

// TestExecutor_CachedHitSkipsRun: rule 1 — apply reports Cached when the
// key hits, even though the plan predicted will-run for cold keys.
func TestExecutor_CachedHitSkipsRun(t *testing.T) {
	counter := &testNodeType{name: "counted", kind: KindAction, outputs: []string{"v"},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			return Outputs{"v": KnownValue("x")}, nil
		}}
	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: hit}
spec:
  nodes:
    - id: a
      type: counted
      config: {}
`
	flow, err := compileWith(t, src, regWith(t, counter))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	res1, err := Apply(context.Background(), flow, store, nilDriver(), ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res1.Get("a"); r.Cached {
		t.Error("cold run must not be cached")
	}
	res2, err := Apply(context.Background(), flow, store, nilDriver(), ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := res2.Get("a")
	if !ok || !r.Cached || r.Status != StatusSucceeded {
		t.Fatalf("warm run = %+v, want succeeded+cached", r)
	}
	if counter.runCount.Load() != 1 {
		t.Errorf("Run called %d times, want 1", counter.runCount.Load())
	}
}

// TestExecutor_ParallelismBound: no more than N nodes run concurrently.
func TestExecutor_ParallelismBound(t *testing.T) {
	var inFlight, peak atomic.Int32
	worker := &testNodeType{name: "work", kind: KindAction, outputs: []string{},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			inFlight.Add(-1)
			return Outputs{}, nil
		}}
	// Six independent nodes.
	var b strings.Builder
	b.WriteString(`apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: par}
spec:
  nodes:
`)
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "    - id: n%d\n      type: work\n      config: {}\n", i)
	}
	flow, err := compileWith(t, b.String(), regWith(t, worker))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), flow, NewMemoryStore(), nilDriver(),
		ExecutorOptions{Parallelism: 2}); err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p > 2 {
		t.Errorf("peak parallelism = %d, want <= 2", p)
	}
	if p := peak.Load(); p < 2 {
		t.Logf("peak = %d (timing-dependent; not a failure)", p)
	}
}

// helpers ----------------------------------------------------------------

// TestFormatChanges covers the Detail summary rules: empty → "no changes",
// ≤3 → past-tensed "created|updated Kind/Name" joined by ", ", >3 → first
// three plus "+N more".
func TestFormatChanges(t *testing.T) {
	diff := func(op driver.Op, kind, name string) driver.Diff {
		return driver.Diff{Op: op, Object: driver.Object{Kind: kind, Name: name}}
	}
	cases := []struct {
		name    string
		changed []driver.Diff
		want    string
	}{
		{"empty", nil, "no changes"},
		{"one create", []driver.Diff{diff(driver.OpCreate, "ConfigMap", "hello")},
			"created ConfigMap/hello"},
		{"one update", []driver.Diff{diff(driver.OpUpdate, "Deployment", "web")},
			"updated Deployment/web"},
		{"two mixed", []driver.Diff{
			diff(driver.OpCreate, "ConfigMap", "a"),
			diff(driver.OpUpdate, "Service", "b"),
		}, "created ConfigMap/a, updated Service/b"},
		{"three exact", []driver.Diff{
			diff(driver.OpCreate, "ConfigMap", "a"),
			diff(driver.OpCreate, "ConfigMap", "b"),
			diff(driver.OpCreate, "ConfigMap", "c"),
		}, "created ConfigMap/a, created ConfigMap/b, created ConfigMap/c"},
		{"four truncates", []driver.Diff{
			diff(driver.OpCreate, "ConfigMap", "a"),
			diff(driver.OpUpdate, "ConfigMap", "b"),
			diff(driver.OpCreate, "ConfigMap", "c"),
			diff(driver.OpCreate, "ConfigMap", "d"),
		}, "created ConfigMap/a, updated ConfigMap/b, created ConfigMap/c, +1 more"},
		{"five truncates", []driver.Diff{
			diff(driver.OpCreate, "ConfigMap", "a"),
			diff(driver.OpCreate, "ConfigMap", "b"),
			diff(driver.OpCreate, "ConfigMap", "c"),
			diff(driver.OpCreate, "ConfigMap", "d"),
			diff(driver.OpCreate, "ConfigMap", "e"),
		}, "created ConfigMap/a, created ConfigMap/b, created ConfigMap/c, +2 more"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatChanges(tc.changed); got != tc.want {
				t.Errorf("formatChanges() = %q, want %q", got, tc.want)
			}
		})
	}
}

// nilDriver returns a driver that fails on use; action-only tests pass it
// and never touch it.
func nilDriver() *panicDriver { return &panicDriver{} }

type panicDriver struct{}

func (p *panicDriver) Plan(ctx context.Context, desired []driver.Object) ([]driver.Diff, error) {
	panic("driver used in an action-only test")
}
func (p *panicDriver) Apply(ctx context.Context, diffs []driver.Diff) error {
	panic("driver used in an action-only test")
}

// compileWith parses+compiles with an explicit registry (engine rule 5).
func compileWith(t *testing.T, src string, reg *Registry) (*CompiledFlow, error) {
	t.Helper()
	flow, err := schema.ParseBytes([]byte(src), "flow.yaml")
	if err != nil {
		return nil, err
	}
	return Compile(flow, reg)
}
