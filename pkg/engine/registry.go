package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/schema"
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

// InputCacher is the optional extension ActionType for nodes whose output
// depends on state outside their own config — external file contents, the
// flow target — that must invalidate the cache when it changes. cfg is the
// node's resolved config (its paths, and so on) and in carries what config
// cannot express (the flow target); together they are enough for the node
// to hash its external state. The node returns a canonical string; the
// engine composes it into the key as {config, extra} (rule 3), so the node
// owns staleness and the engine owns composition. Only action nodes are
// consulted — resource nodes never cache in Phase 3. Errors are
// asymmetric by design: plan degrades the entry to will-run with the
// reason (plan never executes), apply fails the node.
type InputCacher interface {
	CacheInputs(cfg any, in Inputs) (extra string, err error)
}

// MaterializedNode is an action node whose output can live in a file
// that plan and apply read instead of re-running the node. The engine
// passes the node's marked config (out is required to be a literal, so
// resolution state never matters for the path) plus the flow Inputs.
// A non-empty path opts into the materialized workflow: RenderActions
// writes the Run output there with a provenance header, and plan/apply
// read the file back as the node's outputs — a hand-edit wins over any
// cache. Returning an empty path keeps today's ephemeral behavior.
// Implementations should declare exactly one output; the engine sets
// every declared output to the file's contents.
type MaterializedNode interface {
	NodeType
	MaterializedPath(cfg any, in Inputs) (string, error)
}

// ResourceType declares what should exist in the cluster. It never talks
// to a driver: the engine diffs Desired's objects itself.
type ResourceType interface {
	NodeType
	Desired(ctx context.Context, cfg any, in Inputs) ([]driver.Object, error)
}

// Inputs carries what a node's own config cannot express: upstream
// outputs at plan time, the flow file's location for file-relative
// paths, and the flow's target for nodes that render cluster objects.
type Inputs struct {
	// Upstream maps node ID → that node's outputs from this run. Plan
	// time only: apply resolves config before Run/Desired, so they see
	// it empty.
	Upstream map[string]Outputs
	// FlowFile is the path the flow was read from. File-relative nodes
	// resolve paths against its directory — never the process cwd.
	FlowFile string
	// Target is the flow's spec.target (context/namespace).
	Target schema.Target
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
