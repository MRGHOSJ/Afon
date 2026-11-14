package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/release"
	"golang.org/x/sync/errgroup"
)

// ExecutorOptions tunes one apply run.
type ExecutorOptions struct {
	// Parallelism caps concurrent nodes (CLI --parallelism, default 4).
	Parallelism int
	// Wait blocks each resource node until the objects it created or
	// updated are ready (CLI --no-wait disables). Drivers that do not
	// implement driver.Waiter never wait.
	Wait bool
	// WaitTimeout bounds each wait (CLI --wait-timeout, default 5m);
	// <= 0 means no deadline — the apply context bounds the wait.
	WaitTimeout time.Duration
	// Backend writes one release record per apply when non-nil (the
	// k8s CLI wires the Secret backend; the fake driver passes nil and
	// no records exist). A backend outage other than concurrency
	// degrades to a warning — apply never depends on record-keeping.
	Backend release.Backend
	// Trigger labels the record: "" defaults to manual; rollback paths
	// pass release.TriggerRollback.
	Trigger release.Trigger
	// Takeover lets the record's Start supersede a stale pending
	// record (CLI --takeover). Fresh pending records still block.
	Takeover bool
	// Prune lets the run delete recorded objects this flow no longer
	// desires (inventory prune; CLI --no-prune clears it). Separate
	// from Backend because records are written even when pruning is
	// off. A record-read failure during prune degrades to a warning:
	// apply never depends on record-keeping.
	Prune bool
	// RolledBackTo names the release a rollback replayed (0 = not a
	// rollback); it rides the record as the append-only audit trail.
	RolledBackTo int
	// Cluster is the optional cluster runner handed to ClusterAware
	// action nodes on a cache miss (nil for plan-side machinery, the
	// fake driver, and tests without a cluster; see ClusterAware).
	Cluster any
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
//   - a cancelled node records cancelled, not failed;
//   - when opts.Wait is set and the driver implements driver.Waiter,
//     each resource node waits for the objects it created or updated
//     before completing (rule: wait failures fail the node);
//   - when opts.Backend is set, one release record per apply is
//     reserved before any node runs (a concurrent apply is refused
//     first, touching nothing) and promoted to succeeded/failed after
//     the run — records are best-effort: their failures warn, never
//     fail the apply.
func Apply(ctx context.Context, cf *CompiledFlow, store Store, d driver.Driver, opts ExecutorOptions) (res *RunResult, err error) {
	if opts.Parallelism <= 0 {
		opts.Parallelism = 4
	}

	st := &runState{
		remaining:  map[string]int{},
		dependents: map[string][]string{},
		scheduled:  map[string]bool{},
		res:        newRunResult(),
	}

	// Release record: reserve the sequence before scheduling so a
	// concurrent apply is refused before any node runs. Only the
	// concurrency refusal fails the apply; every other backend error
	// degrades to a warning and the run proceeds without a record.
	var pending *release.Record
	if opts.Backend != nil {
		trigger := opts.Trigger
		if trigger == "" {
			trigger = release.TriggerManual
		}
		started, startErr := opts.Backend.Start(ctx, release.Record{
			APIVersion:   release.APIVersion,
			Flow:         cf.Name,
			Trigger:      trigger,
			RolledBackTo: opts.RolledBackTo,
		}, opts.Takeover)
		var conc *release.ErrConcurrentApply
		switch {
		case startErr == nil:
			pending = &started
		case errors.As(startErr, &conc):
			return st.res, startErr
		default:
			st.res.AddWarning(fmt.Sprintf("release record not started: %v", startErr))
		}
	}
	if pending != nil {
		defer func() {
			finalizeRecord(st.res, opts, d, *pending, err)
		}()
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
			out := runNode(gctx, cf, id, st.res, store, d, opts)
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
// from failed. opts carries the wait policy into resource nodes.
func runNode(ctx context.Context, cf *CompiledFlow, id string, res *RunResult, store Store, d driver.Driver, opts ExecutorOptions) NodeResult {
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
		// ClusterAware nodes get their cluster when one was attached;
		// otherwise Run's no-cluster path answers (clear error).
		var outs Outputs
		var runErr error
		if ca, isCluster := nt.(ClusterAware); isCluster && opts.Cluster != nil {
			outs, runErr = ca.RunCluster(ctx, resolved.Value, in, opts.Cluster)
		} else {
			outs, runErr = nt.Run(ctx, resolved.Value, in)
		}
		if runErr != nil {
			if ctx.Err() != nil {
				return NodeResult{NodeID: cn.ID, Status: StatusCancelled, Err: ctx.Err()}
			}
			return NodeResult{NodeID: cn.ID, Status: StatusFailed, Err: runErr}
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
		// Inventory prune: recorded objects this node no longer
		// desires (kind-removal gap) join the label-prune deletes.
		// A record-read failure degrades to a warning — plan already
		// previews the deletes, and apply never depends on
		// record-keeping (ExecutorOptions contract).
		if opts.Backend != nil && opts.Prune {
			merged, pErr := inventoryPruneDiffs(ctx, opts.Backend, d, owner, objs, diffs)
			if pErr != nil {
				res.AddWarning(fmt.Sprintf("inventory prune (node %s): %v", cn.ID, pErr))
			} else {
				diffs = merged
			}
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
		// Capture what this node put into live state for the release
		// record: every desired object (inventory needs OpNone ones
		// too) plus the node's YAML stream. Recorded as soon as the
		// apply succeeds — the objects are live regardless of a later
		// wait failure, and a failed record must inventory them so a
		// rollback can prune them (manifests stay success-only: a
		// failed release is never restorable).
		if manifests := resolvedManifests(resolved); len(objs) > 0 || manifests != "" {
			res.recordApplied(AppliedNode{
				Owner:     owner,
				Objects:   objs,
				Manifests: manifests,
			})
		}
		if len(changed) > 0 {
			// Wait only for objects that came to life or changed —
			// deletes and none-diffs have nothing to become ready.
			if opts.Wait {
				if w, ok := d.(driver.Waiter); ok {
					if objs := waitedObjects(changed); len(objs) > 0 {
						if err := w.Wait(ctx, owner, objs, opts.WaitTimeout); err != nil {
							if ctx.Err() != nil {
								return NodeResult{NodeID: cn.ID, Status: StatusCancelled, Err: ctx.Err()}
							}
							return NodeResult{NodeID: cn.ID, Status: StatusFailed,
								Err: fmt.Errorf("wait: %w", err)}
						}
					}
				}
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

// waitedObjects lifts the create/update diffs out of an applied batch —
// exactly the objects a Waiter should poll. Deletes and none-diffs are
// dropped: there is nothing to wait for.
func waitedObjects(changed []driver.Diff) []driver.Object {
	objs := make([]driver.Object, 0, len(changed))
	for _, df := range changed {
		if df.Op == driver.OpCreate || df.Op == driver.OpUpdate {
			objs = append(objs, df.Object)
		}
	}
	return objs
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

// recordTimeout bounds record promotion. It runs detached from the
// apply's context: a cancelled run must still record what happened.
const recordTimeout = 15 * time.Second

// finalizeRecord promotes the pending record to its terminal status.
// Inventory claims ownership of what is actually live, so it is
// captured for failed runs too — objects their apply put into the
// cluster survive the failure and a later rollback must be able to
// prune them. Manifests stay success-only: a failed release is never a
// rollback target (release.Record.Restorable). A succeeded run captures
// both through driver.ObjectReader. Promotion failures warn — the
// record is bookkeeping, never a reason to fail an apply that already
// finished.
func finalizeRecord(res *RunResult, opts ExecutorOptions, d driver.Driver, pending release.Record, runErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()

	final := pending
	if runErr != nil {
		final.Status = release.StatusFailed
		final.Error = runErr.Error()
	} else {
		final.Status = release.StatusSucceeded
		final.Manifests = manifestStreams(res)
	}
	final.Inventory = captureInventory(ctx, d, res)
	if err := opts.Backend.Promote(ctx, final); err != nil {
		res.AddWarning(fmt.Sprintf("release record: %v", err))
	}
}

// manifestStreams renders each resource node's applied YAML stream in
// node order — completion order is racy under parallelism, and the
// record must be deterministic for tests and diffing.
func manifestStreams(res *RunResult) []release.ManifestStream {
	applied := res.appliedSnapshot()
	out := make([]release.ManifestStream, 0, len(applied))
	for _, a := range applied {
		if a.Manifests != "" {
			out = append(out, release.ManifestStream{Node: a.Owner.Node, YAML: a.Manifests})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// captureInventory reads live state for every object the run applied
// and maps it into record inventory, attributed to the node that
// applied it. A driver without ObjectReader (fake) warns and yields
// empty inventory; a read failure keeps the nodes that succeeded —
// partial inventory beats none, and a warning says so.
func captureInventory(ctx context.Context, d driver.Driver, res *RunResult) []release.InventoryObject {
	reader, ok := d.(driver.ObjectReader)
	if !ok {
		res.AddWarning("release record: driver does not report live state; inventory empty")
		return nil
	}
	var out []release.InventoryObject
	for _, a := range res.appliedSnapshot() {
		if len(a.Objects) == 0 {
			continue
		}
		lives, err := reader.Read(ctx, a.Owner, a.Objects)
		if err != nil {
			res.AddWarning(fmt.Sprintf("release record: reading live state for node %q: %v", a.Owner.Node, err))
			continue
		}
		for _, l := range lives {
			out = append(out, release.InventoryObject{
				Group:     l.Object.Group,
				Version:   l.Object.Version,
				Kind:      l.Object.Kind,
				Namespace: l.Object.Namespace,
				Name:      l.Object.Name,
				Node:      a.Owner.Node,
				UID:       l.UID,
				Hash:      l.Hash,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return out
}

// resolvedManifests lifts the k8s-apply node's "manifests" stream out
// of its resolved config; other resource types contribute none.
func resolvedManifests(resolved ResolveOutcome) string {
	m, ok := resolved.Value.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["manifests"].(string)
	return s
}
