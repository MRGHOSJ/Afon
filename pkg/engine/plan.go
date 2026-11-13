package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

// Plan statuses.
const (
	// PlanCached: action key hit the store; outputs are known.
	PlanCached = "cached"
	// PlanFile: materialized action whose output was read from its
	// rendered file — no cache consulted, the file is the truth.
	PlanFile = "file"
	// PlanWillRun: action will execute (or its key could not be computed
	// because upstream outputs are unknown — an upper bound, engine rule 1).
	PlanWillRun = "will-run"
	// PlanCreate/PlanUpdate/PlanNone/PlanDelete: resource diff results.
	PlanCreate = "create"
	PlanUpdate = "update"
	PlanNone   = "none"
	PlanDelete = "delete"
	// PlanKnownAfterApply: resource config contains an Unknown value.
	PlanKnownAfterApply = "known-after-apply"
)

// PlanEntry is one node's planned treatment.
type PlanEntry struct {
	NodeID string `json:"nodeId"`
	Kind   Kind   `json:"kind"`
	Status string `json:"status"`
	// Reason explains non-obvious statuses (e.g. which upstream node
	// poisoned a known-after-apply entry).
	Reason string `json:"reason,omitempty"`
	// Key is the computed cache key for actions when inputs were known.
	Key string `json:"key,omitempty"`
	// Diff is set for resource entries with a computable diff.
	Diff *driver.Diff `json:"diff,omitempty"`
	// Diffs is set when Desired produced several objects.
	Diffs []driver.Diff `json:"diffs,omitempty"`
}

// Plan is the read-only preview of a flow. It never executes an action.
type Plan struct {
	Flow    string      `json:"flow"`
	File    string      `json:"file,omitempty"`
	Entries []PlanEntry `json:"entries"`
	// Summary counts by status, for a one-line footer.
	Summary map[string]int `json:"summary"`
	// ActionOutputs carries the known action outputs plan already
	// resolved — materialized file contents and cache hits — for CLI
	// transparency (--show-rendered). Not serialized: plan JSON stays
	// exactly what it was before materialized nodes existed.
	ActionOutputs map[string]Outputs `json:"-"`
}

// BuildPlan walks the compiled flow in topological order and decides each
// node's treatment. Actions are never run: a cache hit supplies real
// outputs so downstream nodes plan normally; a miss marks outputs Unknown
// so downstream entries degrade to will-run or known-after-apply
// (design Unknown rules).
func BuildPlan(ctx context.Context, cf *CompiledFlow, store Store, d driver.Driver) (*Plan, error) {
	p := &Plan{
		Flow:          cf.Name,
		File:          cf.File,
		Summary:       map[string]int{},
		ActionOutputs: map[string]Outputs{},
	}
	// upstream maps node ID → outputs known at plan time.
	upstream := map[string]Outputs{}
	// Flow-level Inputs shared by every node: the flow file location and
	// target. Upstream is added per resource node.
	in := Inputs{FlowFile: cf.File, Target: cf.Target}

	for _, id := range cf.Topo {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cn := cf.byID[id]
		lookup := outputsLookup(upstream)
		resolved, err := Resolve(cn.Config, lookup)
		if err != nil {
			return nil, &ConfigError{Pos: cn.IDPos, Message: fmt.Sprintf("node %q: %v", cn.ID, err)}
		}

		var entry PlanEntry
		switch cn.NT.Kind() {
		case KindAction:
			var outs Outputs
			entry, outs, err = planAction(cn, resolved, store, in)
			if err != nil {
				// Materialized file missing (or unreadable): plan fails
				// loudly, naming the node and the fix.
				return nil, &ConfigError{Pos: cn.IDPos, Message: err.Error()}
			}
			upstream[cn.ID] = outs
			switch entry.Status {
			case PlanCached, PlanFile:
				p.ActionOutputs[cn.ID] = outs
			}

		case KindResource:
			entry = planResource(ctx, cf, cn, resolved, d, upstream)
		default:
			return nil, &ConfigError{Pos: cn.IDPos, Message: fmt.Sprintf("node %q: unknown kind %q", cn.ID, cn.NT.Kind())}
		}
		p.Entries = append(p.Entries, entry)
		p.Summary[entry.Status]++
	}
	return p, nil
}

// planAction decides an action's treatment and returns the outputs known to
// downstream nodes at plan time: real ones on a cache hit, the file's
// contents for a materialized node, Unknown on a miss. A key error
// (including a failing InputCacher) degrades to will-run with the reason —
// plan never executes. A materialized node with a missing file is an
// error: the file is the artifact, and falling back to cache or
// will-run would hide that it is gone.
func planAction(cn *CompiledNode, resolved ResolveOutcome, store Store, in Inputs) (PlanEntry, Outputs, error) {
	entry := PlanEntry{NodeID: cn.ID, Kind: KindAction}
	declared := func() Outputs {
		outs := Outputs{}
		for _, name := range cn.NT.DeclaredOutputs() {
			outs[name] = UnknownValue(cn.ID)
		}
		return outs
	}
	// Materialized nodes read their output from the file — checked
	// before the resolved-unknown and cache paths: the file is the
	// truth even when the node's own config refs an uncached upstream
	// (out: is literal, so the path never depends on resolution).
	if outs, path, ok, err := materializedOutputs(cn, in); err != nil {
		return PlanEntry{}, nil, err
	} else if ok {
		entry.Status = PlanFile
		entry.Reason = path
		return entry, outs, nil
	}
	if !resolved.Known {
		// Inputs unknown → key cannot be computed → will run. This is an
		// upper bound: apply may still hit the cache (engine rule 1).
		entry.Status = PlanWillRun
		entry.Reason = fmt.Sprintf("inputs unknown until %q runs", resolved.UnknownFrom)
		return entry, declared(), nil
	}
	key, err := actionCacheKey(cn, resolved, in)
	if err != nil {
		entry.Status = PlanWillRun
		entry.Reason = err.Error()
		return entry, declared(), nil
	}
	entry.Key = key
	outs, ok, err := store.Get(key)
	if err == nil && ok && store.Verify(key, outs) {
		entry.Status = PlanCached
		// A cache hit supplies real outputs, so downstream plans normally.
		return entry, outs, nil
	}
	entry.Status = PlanWillRun
	return entry, declared(), nil
}

func planResource(ctx context.Context, cf *CompiledFlow, cn *CompiledNode, resolved ResolveOutcome, d driver.Driver, upstream map[string]Outputs) PlanEntry {
	entry := PlanEntry{NodeID: cn.ID, Kind: KindResource}
	if !resolved.Known {
		entry.Status = PlanKnownAfterApply
		entry.Reason = fmt.Sprintf("config unknown until %q runs", resolved.UnknownFrom)
		return entry
	}
	rt, ok := cn.NT.(ResourceType)
	if !ok {
		entry.Status = PlanKnownAfterApply
		entry.Reason = fmt.Sprintf("type %q is not a resource", cn.Type)
		return entry
	}
	in := Inputs{Upstream: upstream, FlowFile: cf.File, Target: cf.Target}
	objs, err := rt.Desired(ctx, resolved.Value, in)
	if err != nil {
		entry.Status = PlanKnownAfterApply
		entry.Reason = fmt.Sprintf("desired failed: %v", err)
		return entry
	}
	owner := driver.Owner{Flow: cf.Name, Node: cn.ID}
	diffs, err := d.Plan(ctx, owner, objs)
	if err != nil {
		entry.Status = PlanKnownAfterApply
		entry.Reason = fmt.Sprintf("driver plan failed: %v", err)
		return entry
	}
	// Sort for deterministic output.
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Object.Key() < diffs[j].Object.Key() })
	if len(diffs) == 1 {
		entry.Diff = &diffs[0]
		entry.Status = string(diffs[0].Op)
	} else {
		entry.Diffs = diffs
		entry.Status = worstOp(diffs)
	}
	return entry
}

// worstOp picks the status shown for a multi-object node, risk-ordered:
// a delete is the one destructive outcome and must never hide behind a
// create or update on a plan line (review order: delete > create >
// update > none). Apply executes the opposite way — deletes last — so
// dependents outlive their dependencies; the two orderings are
// deliberate and documented in design.md.
func worstOp(diffs []driver.Diff) string {
	hasDelete, hasCreate, hasUpdate := false, false, false
	for _, df := range diffs {
		switch df.Op {
		case driver.OpDelete:
			hasDelete = true
		case driver.OpCreate:
			hasCreate = true
		case driver.OpUpdate:
			hasUpdate = true
		}
	}
	switch {
	case hasDelete:
		return PlanDelete
	case hasCreate:
		return PlanCreate
	case hasUpdate:
		return PlanUpdate
	}
	return PlanNone
}

func outputsLookup(upstream map[string]Outputs) func(node, output string) (Value, bool) {
	return func(node, output string) (Value, bool) {
		outs, ok := upstream[node]
		if !ok {
			return Value{}, false
		}
		v, ok := outs[output]
		return v, ok
	}
}

// RenderText formats a plan as the CLI's text output. The status column
// is padded to the longest status so every suffix starts at the same
// column: find the status, skip the spaces, and you land on the suffix.
func (p *Plan) RenderText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan for %q (%s)\n", p.Flow, orNone2(p.File))
	maxW := 0
	for _, e := range p.Entries {
		if w := len(e.Status); w > maxW {
			maxW = w
		}
	}
	for _, e := range p.Entries {
		line := fmt.Sprintf("  %-16s %-8s %-*s", e.NodeID, e.Kind, maxW, e.Status)
		switch e.Status {
		case PlanCached:
			line += fmt.Sprintf("  key=%.12s", e.Key)
		case PlanWillRun:
			if e.Key != "" {
				line += fmt.Sprintf("  key=%.12s", e.Key)
			}
		case PlanCreate, PlanUpdate, PlanDelete:
			if e.Diff != nil {
				line += fmt.Sprintf("  %s %s", e.Diff.Object.Kind, e.Diff.Object.Name)
			} else if len(e.Diffs) > 0 {
				line += fmt.Sprintf("  %d objects", len(e.Diffs))
			}
		}
		if e.Reason != "" {
			line += fmt.Sprintf("  (%s)", e.Reason)
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	// Summary line in a stable order; deletes lead the diff group so a
	// destructive plan is visible at a glance (risk-first for review).
	order := []string{PlanCached, PlanFile, PlanWillRun, PlanDelete, PlanCreate, PlanUpdate, PlanNone, PlanKnownAfterApply}
	var parts []string
	for _, s := range order {
		if n := p.Summary[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	fmt.Fprintf(&b, "summary: %s\n", strings.Join(parts, ", "))
	return b.String()
}

// DeleteObjectCount reports how many objects the plan will destroy:
// OpDelete diffs across every entry, counting single-diff entries and
// multi-object batches alike. The summary footer counts entries by
// status — a mixed create+delete batch contributes one "delete" entry
// no matter how many objects it deletes — so the confirmation prompt
// must not read Summary: telling the operator "1 object" while two
// die is exactly the failure the prompt exists to prevent.
func (p *Plan) DeleteObjectCount() int {
	n := 0
	for _, e := range p.Entries {
		if e.Diff != nil && e.Diff.Op == driver.OpDelete {
			n++
		}
		for _, df := range e.Diffs {
			if df.Op == driver.OpDelete {
				n++
			}
		}
	}
	return n
}

func orNone2(s string) string {
	if s == "" {
		return "in-memory"
	}
	return s
}
