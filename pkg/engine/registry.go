package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

// NodeType is the union contract every registered type satisfies: the Base
// part plus exactly one of the kind-specific interfaces below. The kind
// belongs to the type and is registered in code; users never write it
// (design §1).
type NodeType interface {
	// Type is the name users write in flow.yaml, e.g. "image-ref".
	Type() string
	// TypeVersion is an input to the cache key so a change to the node's
	// code invalidates cached results.
	TypeVersion() string
	Kind() Kind
	// Validate checks the structural shape of a config at compile time.
	// cfg may contain Reference and Template markers for ${...} values.
	Validate(cfg any) error
	// DeclaredOutputs lists the output names this type produces; Compile
	// checks every ${node.output} reference against this list.
	DeclaredOutputs() []string
}

// ActionType runs something and produces outputs. Only Run has side
// effects anywhere in the engine, which is what makes plan read-only by
// construction (design §1).
type ActionType interface {
	NodeType
	Run(ctx context.Context, cfg any, in Inputs) (Outputs, error)
}

// ResourceType declares what should exist in the cluster. It never talks
// to a driver: the engine diffs Desired's objects itself.
type ResourceType interface {
	NodeType
	Desired(ctx context.Context, cfg any, in Inputs) ([]driver.Object, error)
}

// Inputs carries resolved upstream outputs for nodes that need more than
// what their own config already references.
type Inputs struct {
	// Upstream maps node ID → that node's outputs from this run.
	Upstream map[string]Outputs
}

// Registry maps type names to implementations. It is explicit — no init()
// registration — so tests can build isolated registries (engine rule 5).
type Registry struct {
	byName map[string]NodeType
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]NodeType{}}
}

// Register adds a node type. Duplicate type names are an error.
func (r *Registry) Register(nt NodeType) error {
	name := nt.Type()
	if name == "" {
		return fmt.Errorf("node type with empty name")
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("node type %q already registered", name)
	}
	r.byName[name] = nt
	return nil
}

// Lookup returns the type registered under name.
func (r *Registry) Lookup(name string) (NodeType, bool) {
	nt, ok := r.byName[name]
	return nt, ok
}

// Names lists registered type names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
