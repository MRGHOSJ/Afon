package engine

import (
	"fmt"
	"sort"

	"github.com/MRGHOSJ/afon/pkg/graph"
	"github.com/MRGHOSJ/afon/pkg/schema"
)

// CompiledNode is one node after type resolution, reference validation,
// config decoding, and graph wiring.
type CompiledNode struct {
	ID    string
	Type  string
	Kind  Kind
	NT    NodeType
	Needs []string
	// Config is the marked config: strings replaced by Reference/Template.
	Config any
	// OnFailure is "halt" (default) or the rejected "rollback".
	OnFailure string
	IDPos     schema.Position
	Refs      []schema.Ref
}

// CompiledFlow is a fully validated flow: types resolved, references
// checked, graph acyclic. This is what validate produces (engine rule 5).
type CompiledFlow struct {
	Name   string
	Target schema.Target
	File   string
	Source []byte
	Nodes  []*CompiledNode
	Graph  *graph.Graph
	Levels map[string]int
	// Topo is a topological order of node IDs.
	Topo []string

	byID map[string]*CompiledNode
}

// Node returns a compiled node by ID.
func (cf *CompiledFlow) Node(id string) (*CompiledNode, bool) {
	n, ok := cf.byID[id]
	return n, ok
}

// Compile validates a parsed flow against a registry: node types, declared
// outputs, needs targets, onFailure, and cycles. It never touches a driver
// (engine rule 5). Errors carry the source position of the offending token.
func Compile(flow *schema.Flow, reg *Registry) (*CompiledFlow, error) {
	if reg == nil {
		return nil, fmt.Errorf("nil registry")
	}
	cf := &CompiledFlow{
		Name:   flow.Name,
		Target: flow.Target,
		File:   flow.File,
		Source: flow.Source,
		byID:   map[string]*CompiledNode{},
	}
	g := graph.New()
	for i := range flow.Nodes {
		src := &flow.Nodes[i]
		nt, ok := reg.Lookup(src.Type)
		if !ok {
			return nil, &ConfigError{
				Pos: src.TypePos,
				Message: fmt.Sprintf("unknown node type %q (registered: %s)",
					src.Type, orNone(reg.Names())),
			}
		}
		cfg, err := DecodeConfig(&src.Config)
		if err != nil {
			return nil, &ConfigError{Pos: src.IDPos, Message: fmt.Sprintf("node %q: %v", src.ID, err)}
		}
		if err := nt.Validate(cfg); err != nil {
			return nil, &ConfigError{Pos: src.IDPos, Message: fmt.Sprintf("node %q: %v", src.ID, err)}
		}
		onFailure := src.OnFailure
		if onFailure == "" {
			onFailure = "halt"
		}
		switch onFailure {
		case "halt":
		case "rollback":
			return nil, &ConfigError{
				Pos:     src.IDPos,
				Message: fmt.Sprintf("node %q: onFailure: rollback not implemented until Phase 4", src.ID),
			}
		default:
			return nil, &ConfigError{
				Pos:     src.IDPos,
				Message: fmt.Sprintf("node %q: unknown onFailure %q (want halt or rollback)", src.ID, onFailure),
			}
		}
		needs := make([]string, 0, len(src.Needs))
		for _, n := range src.Needs {
			needs = append(needs, n.ID)
		}
		cn := &CompiledNode{
			ID:        src.ID,
			Type:      src.Type,
			Kind:      nt.Kind(),
			NT:        nt,
			Needs:     needs,
			Config:    cfg,
			OnFailure: onFailure,
			IDPos:     src.IDPos,
			Refs:      refsOf(flow.Refs, src.ID),
		}
		cf.Nodes = append(cf.Nodes, cn)
		cf.byID[cn.ID] = cn
		g.AddNode(cn.ID)
	}

	// needs edges (positioned errors) — unknown targets.
	for _, cn := range cf.Nodes {
		for _, src := range flow.Nodes {
			if src.ID != cn.ID {
				continue
			}
			for _, ne := range src.Needs {
				if _, ok := cf.byID[ne.ID]; !ok {
					return nil, &ConfigError{
						Pos:     ne.Pos,
						Message: fmt.Sprintf("node %q needs unknown node %q", cn.ID, ne.ID),
					}
				}
				// Edge A→B means B depends on A; needing X makes X → this.
				if ne.ID == cn.ID {
					return nil, &ConfigError{
						Pos:     ne.Pos,
						Message: fmt.Sprintf("node %q cannot need itself", cn.ID),
					}
				}
				if err := g.AddEdge(ne.ID, cn.ID, graph.EdgeNeeds); err != nil {
					return nil, &ConfigError{
						Pos:     ne.Pos,
						Message: fmt.Sprintf("node %q: %v", cn.ID, err),
					}
				}
			}
		}
	}

	// Reference edges: each ${a.b} is a dependency a → node-with-ref.
	for _, ref := range flow.Refs {
		if ref.Reserved {
			return nil, &ConfigError{
				Pos: ref.Pos,
				Message: fmt.Sprintf("${%s.%s} is not implemented until Phase 3",
					ref.Output, ref.From),
			}
		}
		target, ok := cf.byID[ref.From]
		if !ok {
			return nil, &ConfigError{
				Pos:     ref.Pos,
				Message: fmt.Sprintf("node %q references unknown node %q", ref.NodeID, ref.From),
			}
		}
		if ref.From == ref.NodeID {
			return nil, &ConfigError{
				Pos:     ref.Pos,
				Message: fmt.Sprintf("node %q cannot reference itself", ref.NodeID),
			}
		}
		// Declared outputs come from the target's type.
		if !contains(target.NT.DeclaredOutputs(), ref.Output) {
			return nil, &ConfigError{
				Pos: ref.Pos,
				Message: fmt.Sprintf("node %q: unknown output %q for node %q (declared: %s)",
					ref.NodeID, ref.Output, ref.From, orNone(target.NT.DeclaredOutputs())),
			}
		}
		// The edge is referenced-node → consuming-node (dependency direction).
		if err := g.AddEdge(ref.From, ref.NodeID, graph.EdgeRef); err != nil {
			return nil, &ConfigError{
				Pos:     ref.Pos,
				Message: fmt.Sprintf("node %q: %v", ref.NodeID, err),
			}
		}
	}

	levels, err := g.Level()
	if err != nil {
		return nil, &ConfigError{Pos: schema.Position{File: flow.File, Line: 1, Column: 1}, Message: err.Error()}
	}
	topo, err := g.TopoOrder()
	if err != nil {
		return nil, &ConfigError{Pos: schema.Position{File: flow.File, Line: 1, Column: 1}, Message: err.Error()}
	}
	cf.Graph = g
	cf.Levels = levels
	cf.Topo = topo
	return cf, nil
}

func refsOf(refs []schema.Ref, nodeID string) []schema.Ref {
	var out []schema.Ref
	for _, r := range refs {
		if r.NodeID == nodeID {
			out = append(out, r)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "(none)"
	}
	s := append([]string(nil), list...)
	sort.Strings(s)
	out := ""
	for i, e := range s {
		if i > 0 {
			out += ", "
		}
		out += e
	}
	return out
}
