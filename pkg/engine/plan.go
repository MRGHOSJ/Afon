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
	// PlanWillRun: action will execute (or its key could not be computed
	// because upstream outputs are unknown — an upper bound, engine rule 1).
	PlanWillRun = "will-run"
	// PlanCreate/PlanUpdate/PlanNone: resource diff results.
	PlanCreate = "create"
	PlanUpdate = "update"
	PlanNone   = "none"
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
}

// BuildPlan walks the compiled flow in topological order and decides each
// node's treatment. Actions are never run: a cache hit supplies real
// outputs so downstream nodes plan normally; a miss marks outputs Unknown
// so downstream entries degrade to will-run or known-after-apply
// (design Unknown rules).
func BuildPlan(ctx context.Context, cf *CompiledFlow, store Store, d driver.Driver) (*Plan, error) {
	p := &Plan{
		Flow:    cf.Name,
		File:    cf.File,
		Summary: map[string]int{},
	}
	// upstream maps node ID → outputs known at plan time.
	upstream := map[string]Outputs{}

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
			entry, outs = planAction(cn, resolved, store)
			upstream[cn.ID] = outs

		case KindResource:
			entry = planResource(ctx, cn, resolved, d, upstream)
		default:
			return nil, &ConfigError{Pos: cn.IDPos, Message: fmt.Sprintf("node %q: unknown kind %q", cn.ID, cn.NT.Kind())}
		}
		p.Entries = append(p.Entries, entry)
		p.Summary[entry.Status]++
	}
	return p, nil
}

// planAction decides an action's treatment and returns the outputs known to
// downstream nodes at plan time: real ones on a cache hit, Unknown on a miss.
func planAction(cn *CompiledNode, resolved ResolveOutcome, store Store) (PlanEntry, Outputs) {
	entry := PlanEntry{NodeID: cn.ID, Kind: KindAction}
	declared := func() Outputs {
		outs := Outputs{}
		for _, name := range cn.NT.DeclaredOutputs() {
			outs[name] = UnknownValue(cn.ID)
		}
		return outs
	}
	if !resolved.Known {
		// Inputs unknown → key cannot be computed → will run. This is an
		// upper bound: apply may still hit the cache (engine rule 1).
		entry.Status = PlanWillRun
		entry.Reason = fmt.Sprintf("inputs unknown until %q runs", resolved.UnknownFrom)
		return entry, declared()
	}
	key, err := CacheKey(cn.Type, cn.NT.TypeVersion(), resolved.Value)
	if err != nil {
		entry.Status = PlanWillRun
		entry.Reason = err.Error()
		return entry, declared()
	}
	entry.Key = key
	outs, ok, err := store.Get(key)
	if err == nil && ok && store.Verify(key, outs) {
		entry.Status = PlanCached
		// A cache hit supplies real outputs, so downstream plans normally.
		return entry, outs
	}
	entry.Status = PlanWillRun
	return entry, declared()
}

func planResource(ctx context.Context, cn *CompiledNode, resolved ResolveOutcome, d driver.Driver, upstream map[string]Outputs) PlanEntry {
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
	objs, err := rt.Desired(ctx, resolved.Value, Inputs{Upstream: upstream})
	if err != nil {
		entry.Status = PlanKnownAfterApply
		entry.Reason = fmt.Sprintf("desired failed: %v", err)
		return entry
	}
	diffs, err := d.Plan(ctx, objs)
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

// worstOp picks the most significant op for a multi-object node: any create
// wins over update wins over none.
func worstOp(diffs []driver.Diff) string {
	hasUpdate := false
	for _, df := range diffs {
		switch df.Op {
		case driver.OpCreate:
			return PlanCreate
		case driver.OpUpdate:
			hasUpdate = true
		}
	}
	if hasUpdate {
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
		case PlanCreate, PlanUpdate:
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
	// Summary line in a stable status order.
	order := []string{PlanCached, PlanWillRun, PlanCreate, PlanUpdate, PlanNone, PlanKnownAfterApply}
	var parts []string
	for _, s := range order {
		if n := p.Summary[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	fmt.Fprintf(&b, "summary: %s\n", strings.Join(parts, ", "))
	return b.String()
}

func orNone2(s string) string {
	if s == "" {
		return "in-memory"
	}
	return s
}
