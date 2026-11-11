// Package graph is a pure dependency graph: no YAML, no engine, no cluster.
// It knows nodes, edges, ordering, and cycles, and nothing else.
package graph

import (
	"fmt"
	"sort"
	"strings"
)

// EdgeProvenance says why an edge exists, so cycle and validation errors
// can explain themselves instead of just listing node IDs.
type EdgeProvenance string

const (
	// EdgeNeeds is an explicit `needs:` entry: ordering with no data dependency.
	EdgeNeeds EdgeProvenance = "needs"
	// EdgeRef is a `${node.output}` reference in the config.
	EdgeRef EdgeProvenance = "reference"
)

type edge struct {
	to     string
	reason EdgeProvenance
}

// Graph is a directed acyclic graph over node IDs. The zero value is not
// usable; construct with New.
type Graph struct {
	nodes map[string][]edge // adjacency, insertion-ordered per target list
	indeg map[string]int
	order []string // insertion order of node IDs
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{
		nodes: map[string][]edge{},
		indeg: map[string]int{},
	}
}

// AddNode registers a node. Adding the same ID twice is a programming error
// in the caller (schema already rejects duplicate IDs), but it is not fatal
// here: the second add is ignored.
func (g *Graph) AddNode(id string) {
	if _, ok := g.nodes[id]; ok {
		return
	}
	g.nodes[id] = nil
	g.indeg[id] = 0
	g.order = append(g.order, id)
}

// Has reports whether id is registered.
func (g *Graph) Has(id string) bool {
	_, ok := g.nodes[id]
	return ok
}

// Nodes returns all node IDs in insertion order.
func (g *Graph) Nodes() []string {
	out := make([]string, len(g.order))
	copy(out, g.order)
	return out
}

// AddEdge adds a directed edge from → to. Unknown endpoints are an error
// because the caller (Compile) resolves references against the node set first.
// Duplicate edges between the same pair are collapsed; the first provenance
// wins, and indegree counts each pair once.
func (g *Graph) AddEdge(from, to string, reason EdgeProvenance) error {
	if !g.Has(from) {
		return fmt.Errorf("edge from unknown node %q", from)
	}
	if !g.Has(to) {
		return fmt.Errorf("edge to unknown node %q", to)
	}
	if from == to {
		return fmt.Errorf("self edge on node %q", from)
	}
	for _, e := range g.nodes[from] {
		if e.to == to {
			return nil
		}
	}
	g.nodes[from] = append(g.nodes[from], edge{to: to, reason: reason})
	g.indeg[to]++
	return nil
}

// Edges returns the outgoing edges of from, sorted by target ID for
// deterministic output.
func (g *Graph) Edges(from string) [][2]string {
	es := g.nodes[from]
	out := make([][2]string, 0, len(es))
	for _, e := range es {
		out = append(out, [2]string{e.to, string(e.reason)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// Level assigns every node a level: 0 has no dependencies, and each node's
// level is one past the max level of its predecessors. Nodes in the same
// level have no ordering constraint between them and may run in parallel.
// It errors only on a cycle.
func (g *Graph) Level() (map[string]int, error) {
	indeg := make(map[string]int, len(g.indeg))
	for k, v := range g.indeg {
		indeg[k] = v
	}
	level := make(map[string]int, len(g.order))

	// Seed level 0 in insertion order so results are deterministic.
	var ready []string
	for _, id := range g.order {
		if indeg[id] == 0 {
			ready = append(ready, id)
			level[id] = 0
		}
	}
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		// Sort outgoing edges so level assignment is stable regardless of
		// insertion order of parallel edges.
		es := append([]edge(nil), g.nodes[id]...)
		sort.Slice(es, func(i, j int) bool { return es[i].to < es[j].to })
		for _, e := range es {
			if lv := level[id] + 1; lv > level[e.to] {
				level[e.to] = lv
			}
			indeg[e.to]--
			if indeg[e.to] == 0 {
				ready = append(ready, e.to)
			}
		}
	}
	if len(level) != len(g.order) {
		return nil, g.cycle()
	}
	return level, nil
}

// TopoOrder returns a valid topological ordering (ties broken by insertion
// order). It errors on a cycle.
func (g *Graph) TopoOrder() ([]string, error) {
	indeg := make(map[string]int, len(g.indeg))
	for k, v := range g.indeg {
		indeg[k] = v
	}
	var ready []string
	for _, id := range g.order {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	out := make([]string, 0, len(g.order))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		es := append([]edge(nil), g.nodes[id]...)
		sort.Slice(es, func(i, j int) bool { return es[i].to < es[j].to })
		for _, e := range es {
			indeg[e.to]--
			if indeg[e.to] == 0 {
				ready = append(ready, e.to)
			}
		}
	}
	if len(out) != len(g.order) {
		return nil, g.cycle()
	}
	return out, nil
}

// cycle finds one cycle and reports it as a → b → … → a with provenance.
// It uses DFS over whatever nodes were not consumed by a Kahn pass, which is
// exactly the nodes participating in or downstream of a cycle.
func (g *Graph) cycle() error {
	indeg := make(map[string]int, len(g.indeg))
	for k, v := range g.indeg {
		indeg[k] = v
	}
	// Peel nodes Kahn can consume; the rest are cyclic or downstream.
	live := map[string]bool{}
	for _, id := range g.order {
		if indeg[id] == 0 {
			live[id] = false
		} else {
			live[id] = true
		}
	}
	changed := true
	for changed {
		changed = false
		for _, id := range g.order {
			if !live[id] || indeg[id] != 0 {
				continue
			}
			live[id] = false
			changed = true
			for _, e := range g.nodes[id] {
				if live[e.to] {
					indeg[e.to]--
				}
			}
		}
	}

	var (
		visiting = map[string]bool{}
		done     = map[string]bool{}
		stack    []string
	)
	var dfs func(id string) []string
	dfs = func(id string) []string {
		visiting[id] = true
		stack = append(stack, id)
		es := append([]edge(nil), g.nodes[id]...)
		sort.Slice(es, func(i, j int) bool { return es[i].to < es[j].to })
		for _, e := range es {
			if !live[e.to] && !visiting[e.to] {
				continue
			}
			if done[e.to] {
				continue
			}
			if visiting[e.to] {
				// Found the cycle: from e.to in stack to the end, plus e.to.
				idx := 0
				for i, n := range stack {
					if n == e.to {
						idx = i
						break
					}
				}
				cycle := append(append([]string{}, stack[idx:]...), e.to)
				return cycle
			}
			if res := dfs(e.to); res != nil {
				return res
			}
		}
		stack = stack[:len(stack)-1]
		visiting[id] = false
		done[id] = true
		return nil
	}
	for _, id := range g.order {
		if live[id] && !done[id] && !visiting[id] {
			if c := dfs(id); c != nil {
				return g.describeCycle(c)
			}
		}
	}
	return fmt.Errorf("graph contains a cycle")
}

// describeCycle renders a → b → a, naming the edge provenance where it is
// not the implicit default.
func (g *Graph) describeCycle(cycle []string) error {
	parts := make([]string, 0, len(cycle))
	for i := 0; i+1 < len(cycle); i++ {
		from, to := cycle[i], cycle[i+1]
		reason := EdgeRef
		for _, e := range g.nodes[from] {
			if e.to == to {
				reason = e.reason
				break
			}
		}
		if reason == EdgeRef {
			parts = append(parts, from)
		} else {
			parts = append(parts, fmt.Sprintf("%s (via %s)", from, reason))
		}
	}
	parts = append(parts, cycle[len(cycle)-1])
	return fmt.Errorf("cycle: %s", strings.Join(parts, " → "))
}
