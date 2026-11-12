package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"golang.org/x/sync/errgroup"
)

// ExecutorOptions tunes one apply run.
type ExecutorOptions struct {
	// Parallelism caps concurrent nodes (CLI --parallelism, default 4).
	Parallelism int
}

// runState is the shared mutable state of one Apply: the dependency
// countdown that drives scheduling plus the RunResult every completion
// records into. All access is mutex-protected so concurrent completions
// are safe under -race.
type runState struct {
	mu sync.Mutex
	// remaining counts unfinished dependencies per node.
	remaining map[string]int
	// dependents maps a node to the nodes waiting on it.
	dependents map[string][]string
	// scheduled marks nodes already launched.
	scheduled map[string]bool

	res *RunResult
}

// complete records a node's terminal result and returns the nodes that
// became ready because of it.
func (s *runState) complete(id string, r NodeResult) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.res.Record(r)
	var ready []string
	for _, dep := range s.dependents[id] {
		s.remaining[dep]--
		if s.remaining[dep] == 0 && !s.scheduled[dep] {
			s.scheduled[dep] = true
			ready = append(ready, dep)
		}
	}
	return ready
}

// Apply executes a compiled flow against a driver and cache. It shares no
// state with any previously built plan: callers display a plan, then call
// Apply for fresh execution.
//
// Semantics (design §1 + engine rules):
//   - nodes with satisfied dependencies run in parallel, capped by
//     Parallelism;
//   - the first failure cancels the group: in-flight nodes finish or
//     cancel, no new nodes are scheduled, every node gets exactly one
//     terminal state;
//   - a cache hit short-circuits Run and marks the result Cached
//     (engine rule 1: a plan's will-run is an upper bound);
//   - a cache write happens only after a successful Run (rule 4);
//   - a cancelled node records cancelled, not failed.
func Apply(ctx context.Context, cf *CompiledFlow, store Store, d driver.Driver, opts ExecutorOptions) (*RunResult, error) {
	if opts.Parallelism <= 0 {
		opts.Parallelism = 4
	}

	st := &runState{
		remaining:  map[string]int{},
		dependents: map[string][]string{},
		scheduled:  map[string]bool{},
		res:        newRunResult(),
	}

	// Build the dependency countdown: a graph edge A→B means B depends on
	// A; `needs` entries add the same relation without a data dependency.
	edgeDeps := map[string]map[string]bool{}
	for _, n := range cf.Nodes {
		edgeDeps[n.ID] = map[string]bool{}
	}
	for _, from := range cf.Nodes {
		for _, e := range cf.Graph.Edges(from.ID) {
			if to := e[0]; to != from.ID {
				edgeDeps[to][from.ID] = true
			}
		}
	}
	for _, n := range cf.Nodes {
		deps := edgeDeps[n.ID]
		for _, need := range n.Needs {
			if need != n.ID {
				deps[need] = true
			}
		}
		st.remaining[n.ID] = len(deps)
		for dep := range deps {
			st.dependents[dep] = append(st.dependents[dep], n.ID)
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Parallelism)

	launched := 0
	// readyCh delivers newly-ready batches to the scheduler. Buffered so
	// a halted run never deadlocks a finishing goroutine.
	readyCh := make(chan []string, len(cf.Nodes))
	launch := func(id string) {
		launched++
		g.Go(func() error {
			out := runNode(gctx, cf, id, st.res, store, d)
			ready := st.complete(id, out)
			if out.Status == StatusFailed {
				// Returning the error cancels gctx: no further scheduling,
				// in-flight siblings observe cancellation. The ready batch
				// is dropped: dependents of a failed node never run.
				return out.Err
			}
			readyCh <- ready
			return nil
		})
	}

	// Seed: nodes with no dependencies, in compiled (topological) order.
	// The list is collected before any goroutine exists so the unlocked
	// read of remaining is race-free; scheduled is then set under mu
	// because completions may already be writing it.
	var seed []string
	for _, n := range cf.Nodes {
		if st.remaining[n.ID] == 0 {
			seed = append(seed, n.ID)
		}
	}
	for _, id := range seed {
		st.mu.Lock()
		st.scheduled[id] = true
		st.mu.Unlock()
		launch(id)
	}

	// Scheduler loop runs in this goroutine: g.Go calls and Wait never
	// overlap (sync.WaitGroup requirement), and scheduling stops at the
	// first failure or context cancellation.
	halted := false
	for launched < len(cf.Nodes) && !halted {
		select {
		case ready := <-readyCh:
			if gctx.Err() != nil {
				// First failure already cancelled the run: schedule nothing
				// new; whatever is in flight finishes or cancels.
				halted = true
				break
			}
			for _, id := range ready {
				launch(id)
			}
		case <-gctx.Done():
			halted = true
		}
	}

	waitErr := g.Wait()

	// Anything never scheduled gets a terminal state: skipped when the run
	// halted on a failure, cancelled when the caller's context ended
	// (e.g. Ctrl-C).
	for _, n := range cf.Nodes {
		if _, ok := st.res.Get(n.ID); ok {
			continue
		}
		status := StatusSkipped
		if ctx.Err() != nil {
			status = StatusCancelled
		}
		st.res.Record(NodeResult{NodeID: n.ID, Status: status})
	}

	switch {
	case waitErr != nil && !errors.Is(waitErr, context.Canceled):
		return st.res, waitErr
	case ctx.Err() != nil:
		return st.res, ctx.Err()
	}
	if st.res.Failed {
		for _, id := range cf.Topo {
			if r, ok := st.res.Get(id); ok && r.Status == StatusFailed {
				return st.res, fmt.Errorf("node %q failed: %w", id, r.Err)
			}
		}
	}
	return st.res, nil
}

// runNode executes one node and returns its terminal result. It never
// writes the cache on failure (engine rule 4) and distinguishes cancelled
// from failed.
func runNode(ctx context.Context, cf *CompiledFlow, id string, res *RunResult, store Store, d driver.Driver) NodeResult {
	cn, ok := cf.Node(id)
	if !ok {
		return NodeResult{NodeID: id, Status: StatusFailed, Err: fmt.Errorf("internal: unknown node %q", id)}
	}
	if err := ctx.Err(); err != nil {
		return NodeResult{NodeID: cn.ID, Status: StatusCancelled, Err: err}
	}

	// Dependencies are recorded before launch (complete → ready → launch),
	// so every lookup below sees finished outputs.
	lookup := func(node, output string) (Value, bool) {
		r, ok := res.Get(node)
		if !ok {
			return Value{}, false
		}
		v, ok := r.Outputs[output]
		return v, ok
	}

	// Flow-level Inputs shared by both kinds: the flow file location and
	// target. Plan and apply compute identical InputCacher keys from it.
	in := Inputs{FlowFile: cf.File, Target: cf.Target}
	// Owner stamps driver calls so an inventory driver can scope prune
	// and ownership to this flow + node.
	owner := driver.Owner{Flow: cf.Name, Node: cn.ID}

	// Materialized action nodes: the file is the output — read it and
	// return before Resolve or the cache. Skipping Resolve means an
	// uncached upstream cannot fail a node whose output lives on disk;
	// skipping the cache means a hand-edit always wins over stale
	// cached bytes (engine rule about edits: plan/apply honor them).
	if outs, path, ok, err := materializedOutputs(cn, in); err != nil {
		return NodeResult{NodeID: cn.ID, Status: StatusFailed, Err: err}
	} else if ok {
		return NodeResult{NodeID: cn.ID, Status: StatusSucceeded, Outputs: outs,
			Detail: fmt.Sprintf("from %s", path)}
	}

	resolved, err := Resolve(cn.Config, lookup)
	if err != nil {
		return NodeResult{NodeID: cn.ID, Status: StatusFailed, Err: err}
	}
	if !resolved.Known {
		return NodeResult{
			NodeID: cn.ID,
			Status: StatusFailed,
			Err:    fmt.Errorf("upstream %q did not produce required outputs", resolved.UnknownFrom),
		}
	}

	switch nt := cn.NT.(type) {
	case ActionType:
		key, err := actionCacheKey(cn, resolved, in)
		if err != nil {
			return NodeResult{NodeID: cn.ID, Status: StatusFailed, Err: err}
		}
		// Verified cache hit: skip Run, report Cached (engine rule 1).
		if outs, ok, err := store.Get(key); err == nil && ok && store.Verify(key, outs) {
			return NodeResult{NodeID: cn.ID, Status: StatusSucceeded, Cached: true, Outputs: outs}
		}
		outs, err := nt.Run(ctx, resolved.Value, in)
		if err != nil {
			if ctx.Err() != nil {
				return NodeResult{NodeID: cn.ID, Status: StatusCancelled, Err: ctx.Err()}
			}
			return NodeResult{NodeID: cn.ID, Status: StatusFailed, Err: err}
		}
		// Rule 4: only a successful Run writes the cache.
		if err := store.Put(key, outs); err != nil {
			return NodeResult{NodeID: cn.ID, Status: StatusFailed,
				Err: fmt.Errorf("writing cache: %w", err)}
		}
		return NodeResult{NodeID: cn.ID, Status: StatusSucceeded, Outputs: outs}

	case ResourceType:
		objs, err := nt.Desired(ctx, resolved.Value, in)
		if err != nil {
			return NodeResult{NodeID: cn.ID, Status: StatusFailed, Err: err}
		}
		diffs, err := d.Plan(ctx, owner, objs)
		if err != nil {
			return NodeResult{NodeID: cn.ID, Status: StatusFailed, Err: err}
		}
		changed := make([]driver.Diff, 0, len(diffs))
		for _, df := range diffs {
			if df.Op != driver.OpNone {
				changed = append(changed, df)
			}
		}
		if len(changed) > 0 {
			if err := d.Apply(ctx, owner, changed); err != nil {
				if ctx.Err() != nil {
					return NodeResult{NodeID: cn.ID, Status: StatusCancelled, Err: ctx.Err()}
				}
				return NodeResult{NodeID: cn.ID, Status: StatusFailed, Err: err}
			}
		}
		return NodeResult{NodeID: cn.ID, Status: StatusSucceeded,
			DiffCount: len(changed), Detail: formatChanges(changed)}

	default:
		return NodeResult{
			NodeID: cn.ID,
			Status: StatusFailed,
			Err:    fmt.Errorf("node type %q implements neither ActionType nor ResourceType", cn.Type),
		}
	}
}

// formatChanges summarizes the changed diffs a resource node applied:
// "no changes", up to three past-tensed "created|updated|deleted
// Kind/Name" entries, or the first three plus "+N more" when there are
// more than three.
func formatChanges(changed []driver.Diff) string {
	if len(changed) == 0 {
		return "no changes"
	}
	const max = 3
	n := len(changed)
	if n > max {
		n = max
	}
	parts := make([]string, 0, n+1)
	for _, df := range changed[:n] {
		verb := "updated"
		switch df.Op {
		case driver.OpCreate:
			verb = "created"
		case driver.OpDelete:
			verb = "deleted"
		}
		parts = append(parts, fmt.Sprintf("%s %s/%s", verb, df.Object.Kind, df.Object.Name))
	}
	if len(changed) > max {
		parts = append(parts, fmt.Sprintf("+%d more", len(changed)-max))
	}
	return strings.Join(parts, ", ")
}
