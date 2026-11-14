package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// sentinelCluster is the payload type ClusterAware nodes type-assert;
// tests pass instances (or wrong types) through ExecutorOptions.Cluster.
type sentinelCluster struct{ id string }

// clusterStub implements NodeType + ActionType + ClusterAware with
// call counters so dispatch can be asserted precisely.
type clusterStub struct {
	name         string
	outputs      []string
	runErr       error
	assertRunner bool

	runCalls     atomic.Int32
	clusterCalls atomic.Int32

	mu          sync.Mutex
	lastCluster any
}

func (s *clusterStub) Type() string              { return s.name }
func (s *clusterStub) TypeVersion() string       { return "1" }
func (s *clusterStub) Kind() Kind                { return KindAction }
func (s *clusterStub) DeclaredOutputs() []string { return s.outputs }

func (s *clusterStub) Validate(cfg any) error { return nil }

func (s *clusterStub) Run(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
	s.runCalls.Add(1)
	if s.runErr != nil {
		return nil, s.runErr
	}
	return Outputs{"v": KnownValue("from-run")}, nil
}

func (s *clusterStub) RunCluster(ctx context.Context, cfg any, in Inputs, cluster any) (Outputs, error) {
	s.clusterCalls.Add(1)
	s.mu.Lock()
	s.lastCluster = cluster
	s.mu.Unlock()
	if s.assertRunner {
		if _, ok := cluster.(*sentinelCluster); !ok {
			return nil, fmt.Errorf("unexpected cluster runner type %T", cluster)
		}
	}
	return Outputs{"v": KnownValue("from-cluster")}, nil
}

func compileClusterFlow(t *testing.T, nt NodeType) *CompiledFlow {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register(nt); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: cluster-aware}
spec:
  nodes:
    - id: n
      type: %s
      config: {x: "1"}
`, nt.Type())
	flow, err := compileWith(t, src, reg)
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

// Amendment: a ClusterAware node on a cache miss with no cluster must
// fall through to Run, whose clear error reaches the caller.
func TestClusterAware_NilClusterFallsBackToRun(t *testing.T) {
	stub := &clusterStub{
		name: "cstub", outputs: []string{"v"},
		runErr: errors.New("image-build: requires the k8s driver (apply only)"),
	}
	flow := compileClusterFlow(t, stub)
	res, err := Apply(context.Background(), flow, NewMemoryStore(), nilDriver(), ExecutorOptions{})
	if err == nil {
		t.Fatal("expected the node to fail without a cluster")
	}
	nr, ok := res.Get("n")
	if !ok {
		t.Fatal("missing node result")
	}
	if nr.Status != StatusFailed {
		t.Errorf("status = %s, want failed", nr.Status)
	}
	if nr.Err == nil || !strings.Contains(nr.Err.Error(), "requires the k8s driver (apply only)") {
		t.Errorf("err = %v, want the clear no-driver message", nr.Err)
	}
	if got := stub.clusterCalls.Load(); got != 0 {
		t.Errorf("RunCluster called %d times without a cluster", got)
	}
	if got := stub.runCalls.Load(); got != 1 {
		t.Errorf("Run calls = %d, want 1", got)
	}
}

// With a cluster attached, the miss path must call RunCluster with the
// exact payload and never Run.
func TestClusterAware_UsesRunClusterWhenClusterSet(t *testing.T) {
	stub := &clusterStub{name: "cstub", outputs: []string{"v"}}
	flow := compileClusterFlow(t, stub)
	sentinel := &sentinelCluster{id: "runner-1"}
	res, err := Apply(context.Background(), flow, NewMemoryStore(), nilDriver(), ExecutorOptions{Cluster: sentinel})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	nr, _ := res.Get("n")
	if nr.Status != StatusSucceeded {
		t.Errorf("status = %s, want succeeded", nr.Status)
	}
	if v := nr.Outputs["v"]; !v.Known || v.Data != "from-cluster" {
		t.Errorf("output v = %#v, want from-cluster", v)
	}
	if got := stub.clusterCalls.Load(); got != 1 {
		t.Errorf("RunCluster calls = %d, want 1", got)
	}
	if got := stub.runCalls.Load(); got != 0 {
		t.Errorf("Run calls = %d, want 0", got)
	}
	stub.mu.Lock()
	got := stub.lastCluster
	stub.mu.Unlock()
	if got != any(sentinel) {
		t.Errorf("payload = %#v, want the sentinel", got)
	}
}

// Amendment: a wrong payload type must produce a clear error from the
// node, not a panic, and the engine must surface it as the node error.
func TestClusterAware_WrongRunnerTypeClearError(t *testing.T) {
	stub := &clusterStub{name: "cstub", outputs: []string{"v"}, assertRunner: true}
	flow := compileClusterFlow(t, stub)
	res, err := Apply(context.Background(), flow, NewMemoryStore(), nilDriver(), ExecutorOptions{Cluster: "not-a-runner"})
	if err == nil {
		t.Fatal("expected failure on wrong runner type")
	}
	nr, _ := res.Get("n")
	if nr.Err == nil || !strings.Contains(nr.Err.Error(), "unexpected cluster runner type string") {
		t.Errorf("err = %v, want the clear type error", nr.Err)
	}
	if got := stub.runCalls.Load(); got != 0 {
		t.Errorf("Run calls = %d, want 0 (wrong payload never falls back to Run)", got)
	}
}

// Amendment: a verified cache hit short-circuits before dispatch —
// the cluster is never touched on the second apply.
func TestClusterAware_CacheHitNeverTouchesCluster(t *testing.T) {
	stub := &clusterStub{name: "cstub", outputs: []string{"v"}}
	flow := compileClusterFlow(t, stub)
	store := NewMemoryStore()
	res1, err := Apply(context.Background(), flow, store, nilDriver(), ExecutorOptions{Cluster: &sentinelCluster{id: "first"}})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if nr, _ := res1.Get("n"); nr.Cached {
		t.Fatal("first apply must not be cached")
	}
	if got := stub.clusterCalls.Load(); got != 1 {
		t.Fatalf("RunCluster calls after first apply = %d, want 1", got)
	}

	res2, err := Apply(context.Background(), flow, store, nilDriver(), ExecutorOptions{Cluster: &sentinelCluster{id: "second"}})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	nr, _ := res2.Get("n")
	if nr.Status != StatusSucceeded || !nr.Cached {
		t.Errorf("second apply = %s cached=%v, want succeeded+cached", nr.Status, nr.Cached)
	}
	if got := stub.clusterCalls.Load(); got != 1 {
		t.Errorf("RunCluster calls after cache hit = %d, want still 1", got)
	}
	if got := stub.runCalls.Load(); got != 0 {
		t.Errorf("Run calls = %d, want 0", got)
	}
}

// A plain ActionType node (no ClusterAware) keeps its normal Run path
// even when a cluster is attached.
func TestClusterAware_PlainNodeIgnoresCluster(t *testing.T) {
	plain := &testNodeType{name: "plain", kind: KindAction, outputs: []string{"v"},
		run: func(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
			return Outputs{"v": KnownValue("ok")}, nil
		}}
	reg := NewRegistry()
	if err := reg.Register(plain); err != nil {
		t.Fatal(err)
	}
	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: plain-cluster}
spec:
  nodes:
    - id: n
      type: plain
      config: {}
`
	flow, err := compileWith(t, src, reg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Apply(context.Background(), flow, NewMemoryStore(), nilDriver(), ExecutorOptions{Cluster: &sentinelCluster{id: "x"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	nr, _ := res.Get("n")
	if nr.Status != StatusSucceeded {
		t.Errorf("status = %s, want succeeded", nr.Status)
	}
	if got := plain.runCount.Load(); got != 1 {
		t.Errorf("Run calls = %d, want 1", got)
	}
}
