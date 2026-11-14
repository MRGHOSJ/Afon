package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/graph"
	"github.com/MRGHOSJ/afon/pkg/release"
)

// RollbackPlan builds the dry-run preview for rolling back to release
// seq: the release's recorded manifests replay as resource nodes under
// their recorded node ids (design §3), planned against live state with
// the same inventory prune apply uses. The plan therefore shows
// exactly what re-applying seq would change — creates/updates for
// drifted objects, deletes for everything the flow owns now that seq
// never had (label prune plus the newest inventory's prune).
func RollbackPlan(ctx context.Context, cf *CompiledFlow, backend release.Backend, d driver.Driver, seq int) (*Plan, error) {
	rec, err := rollbackRecord(ctx, cf, backend, seq)
	if err != nil {
		return nil, err
	}
	synth, err := rollbackFlow(cf, rec)
	if err != nil {
		return nil, err
	}
	return BuildPlanOpts(ctx, synth, NewMemoryStore(), d, PlanOptions{Backend: backend})
}

// Rollback re-applies release seq's recorded manifests through the
// normal apply path: per-node plan and prune, readiness waiting, and
// one new release record — trigger rollback, rolledBackTo seq — so
// history stays append-only. The flow file's current content never
// participates; the recorded manifests are the ground truth.
func Rollback(ctx context.Context, cf *CompiledFlow, store Store, d driver.Driver, seq int, opts ExecutorOptions) (*RunResult, error) {
	rec, err := rollbackRecord(ctx, cf, opts.Backend, seq)
	if err != nil {
		return nil, err
	}
	synth, err := rollbackFlow(cf, rec)
	if err != nil {
		return nil, err
	}
	opts.Trigger = release.TriggerRollback
	opts.RolledBackTo = seq
	return Apply(ctx, synth, store, d, opts)
}

// rollbackRecord fetches and sanity-checks the release seq replay would
// use. The backend is flow-scoped, but the engine still verifies the
// record it got back belongs to this flow.
func rollbackRecord(ctx context.Context, cf *CompiledFlow, backend release.Backend, seq int) (release.Record, error) {
	if backend == nil {
		return release.Record{}, errors.New("rollback needs a release backend")
	}
	if seq < 1 {
		return release.Record{}, fmt.Errorf("release sequence must be >= 1, got %d", seq)
	}
	rec, ok, err := backend.Get(ctx, seq)
	if err != nil {
		return release.Record{}, fmt.Errorf("reading release v%d: %w", seq, err)
	}
	if !ok {
		return release.Record{}, fmt.Errorf("release v%d does not exist", seq)
	}
	if rec.Flow != cf.Name {
		return release.Record{}, fmt.Errorf("release v%d belongs to flow %q, not %q", seq, rec.Flow, cf.Name)
	}
	return rec, nil
}

// rollbackFlow synthesizes the replay flow: one resource node per
// recorded manifest stream, id preserved so owners and inventory
// attribution match what the release recorded. Streams without YAML
// replay nothing (only synthetic records without manifests carry
// them); a release with nothing usable to replay is refused.
func rollbackFlow(cf *CompiledFlow, rec release.Record) (*CompiledFlow, error) {
	if !rec.Restorable() {
		return nil, fmt.Errorf("release v%d: status %q has nothing to restore (roll back to a succeeded or rolledback release)",
			rec.Sequence, rec.Status)
	}
	rb := &CompiledFlow{
		Name:   cf.Name,
		Target: cf.Target,
		File:   cf.File,
		Graph:  graph.New(),
		byID:   map[string]*CompiledNode{},
	}
	for _, m := range rec.Manifests {
		if strings.TrimSpace(m.YAML) == "" {
			continue
		}
		if m.Node == "" {
			return nil, fmt.Errorf("release v%d: manifest stream without a node id", rec.Sequence)
		}
		if _, dup := rb.byID[m.Node]; dup {
			return nil, fmt.Errorf("release v%d: duplicate manifest stream for node %q", rec.Sequence, m.Node)
		}
		cn := &CompiledNode{
			ID:     m.Node,
			Type:   "k8s-apply",
			Kind:   KindResource,
			NT:     K8sApply{},
			Config: map[string]any{"manifests": m.YAML},
		}
		rb.Nodes = append(rb.Nodes, cn)
		rb.Topo = append(rb.Topo, m.Node)
		rb.byID[m.Node] = cn
		// Recorded streams carry no dependency information, so replay
		// treats them as independent — the same parallel apply the
		// original flow used for unconnected nodes. An order-sensitive
		// restore fails loudly and is retryable.
		rb.Graph.AddNode(m.Node)
	}
	if len(rb.Nodes) == 0 {
		return nil, fmt.Errorf("release v%d: no manifests recorded (nothing to replay)", rec.Sequence)
	}
	return rb, nil
}

// DefaultRollbackTarget resolves the release an implicit rollback
// should replay: the newest restorable release other than the latest
// record — rolling back "to before whatever is live now". It never
// targets a failed or pending record.
func DefaultRollbackTarget(ctx context.Context, backend release.Backend, flow string) (int, error) {
	records, err := backend.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading release records: %w", err)
	}
	if len(records) == 0 {
		return 0, fmt.Errorf("no releases recorded for flow %q", flow)
	}
	// List is newest-first; skip the latest (index 0) by definition.
	for _, r := range records[1:] {
		if r.Restorable() {
			return r.Sequence, nil
		}
	}
	return 0, fmt.Errorf("no earlier release to roll back to for flow %q", flow)
}

// AutoRollbackTarget reports which release an automatic rollback
// after this run should replay: 0 — no rollback due — when the run did
// not fail, no failed node declared onFailure: rollback, or there is
// no backend (the fake driver has no releases to replay). An error is
// returned only when a rollback IS due and its target cannot be
// resolved (records unreadable, or nothing earlier to replay).
func AutoRollbackTarget(ctx context.Context, cf *CompiledFlow, backend release.Backend, res *RunResult) (int, error) {
	if backend == nil || res == nil || !res.Failed {
		return 0, nil
	}
	armed := false
	for _, n := range cf.Nodes {
		if n.OnFailure != "rollback" {
			continue
		}
		if r, ok := res.Get(n.ID); ok && r.Status == StatusFailed {
			armed = true
			break
		}
	}
	if !armed {
		return 0, nil
	}
	return DefaultRollbackTarget(ctx, backend, cf.Name)
}
