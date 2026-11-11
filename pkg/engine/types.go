package engine

import (
	"fmt"
	"sync"

	"github.com/MRGHOSJ/afon/pkg/schema"
)

// Kind classifies a node type; the type registers it, users never write it.
type Kind string

const (
	KindAction   Kind = "action"
	KindResource Kind = "resource"
)

// Value is one output value with an explicit known/unknown state. Unknown is
// not an error: it means "will exist after apply" (design rule 2).
type Value struct {
	Known bool `json:"known"`
	Data  any  `json:"data,omitempty"`
	// From names the node whose run produced the Unknown, for plan reasons.
	// Empty for known values.
	From string `json:"from,omitempty"`
}

// KnownValue wraps a concrete value.
func KnownValue(v any) Value { return Value{Known: true, Data: v} }

// UnknownValue marks a value that does not exist yet.
func UnknownValue(from string) Value { return Value{Known: false, From: from} }

// Outputs are a node's declared outputs. A map key present with Known=false
// exists but has no value yet; an absent key is a contract violation by the
// node type (Compile checked DeclaredOutputs against references).
type Outputs map[string]Value

// Reference is a whole-string `${node.output}` marker inside a decoded
// config. It keeps the referenced value's type at resolution time.
type Reference struct {
	Node   string
	Output string
	// Reserved is true for ${secret.NAME}/${env.NAME}; Compile rejects
	// those in Phase 2.
	Reserved bool
	// Name holds the reserved form's NAME.
	Name string
}

// Template is a string with interpolation: parts are string literals or
// References. The resolved value is always a string.
type Template struct {
	Parts []any // string | Reference
}

// ConfigError is a compile-time failure tied to a source position.
type ConfigError struct {
	Pos     schema.Position
	Message string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Message)
}

// NodeStatus is a terminal state in a run result.
type NodeStatus string

const (
	StatusSucceeded NodeStatus = "succeeded"
	StatusFailed    NodeStatus = "failed"
	StatusSkipped   NodeStatus = "skipped"
	StatusCancelled NodeStatus = "cancelled"
)

// NodeResult is what actually happened to one node during apply. It is
// deliberately separate from the release record (design §3) so Phase 4 can
// embed it without a rewrite.
type NodeResult struct {
	NodeID string     `json:"nodeId"`
	Status NodeStatus `json:"status"`
	// Cached is true when an action hit the cache instead of running.
	// A plan's "will-run" is an upper bound: apply reports the truth
	// (engine rule 1).
	Cached    bool    `json:"cached,omitempty"`
	Outputs   Outputs `json:"outputs,omitempty"`
	Err       error   `json:"-"`
	ErrText   string  `json:"error,omitempty"`
	DiffCount int     `json:"diffCount,omitempty"`
	// Detail is a short human summary of what the node did, e.g.
	// "created ConfigMap/hello" or "no changes". Rendered by the CLI
	// after the status column.
	Detail string `json:"detail,omitempty"`
}

// RunResult is the outcome of one apply. All writes go through Record so
// concurrent node completions are safe.
type RunResult struct {
	mu    sync.Mutex
	Nodes map[string]NodeResult
	// Failed is true when at least one node failed (as opposed to the run
	// being cancelled by a signal).
	Failed bool
}

func newRunResult() *RunResult {
	return &RunResult{Nodes: map[string]NodeResult{}}
}

// Record stores or replaces a node's result. Safe for concurrent use.
func (r *RunResult) Record(res NodeResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if res.Status == StatusFailed {
		r.Failed = true
	}
	if res.Err != nil {
		res.ErrText = res.Err.Error()
	}
	r.Nodes[res.NodeID] = res
}

// Get returns a node's result.
func (r *RunResult) Get(id string) (NodeResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.Nodes[id]
	return res, ok
}

// Snapshot copies all results for safe iteration after the run.
func (r *RunResult) Snapshot() map[string]NodeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]NodeResult, len(r.Nodes))
	for k, v := range r.Nodes {
		out[k] = v
	}
	return out
}
