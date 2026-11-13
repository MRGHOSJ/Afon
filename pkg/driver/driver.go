// Package driver is the contract between the engine and whatever executes
// resource changes. It is a leaf package: no imports from engine, graph, or
// schema, so both the engine and any driver implementation depend on it and
// never on each other.
package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Object is a single desired cluster object in Phase 2 terms: identity plus
// a data payload. Data is compared and hashed in canonical form only (design
// rule 7), so map ordering and 1 vs 1.0 never produce phantom diffs.
type Object struct {
	Group     string         `json:"group,omitempty"`
	Version   string         `json:"version,omitempty"`
	Kind      string         `json:"kind"`
	Namespace string         `json:"namespace,omitempty"`
	Name      string         `json:"name"`
	Data      map[string]any `json:"data,omitempty"`
}

// Key is the identity of an object: group/version/kind/namespace/name.
// Two objects with the same key are the same object for diffing purposes.
func (o Object) Key() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", o.Group, o.Version, o.Kind, o.Namespace, o.Name)
}

// Op is what a diff says must happen to one object.
type Op string

const (
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpNone   Op = "none"
	// OpDelete: a live object owned by this Owner exists but is absent
	// from desired — the driver must prune it (design §4 inventory).
	OpDelete Op = "delete"
)

// Owner identifies who a Plan/Apply call belongs to: the flow plus the
// node within it. Drivers with inventory (the Phase 3 Kubernetes driver)
// label applied objects with it and prune only what the same owner
// claims, so two apply nodes in one flow never delete each other's
// objects. Drivers without inventory (fake) accept and ignore it.
type Owner struct {
	Flow string `json:"flow"`
	Node string `json:"node"`
}

// Diff is one object's planned change.
type Diff struct {
	Object Object `json:"object"`
	Op     Op     `json:"op"`
}

// Driver plans and applies resource changes. Plan is strictly read-only.
type Driver interface {
	// Plan diffs desired objects against live state, scoped to owner:
	// objects the owner already owns but no longer desires become delete
	// diffs, and objects owned by a different flow/node are refused with
	// ErrForeignOwner rather than adopted. It must not mutate anything,
	// including the driver's own persisted state.
	Plan(ctx context.Context, owner Owner, desired []Object) ([]Diff, error)
	// Apply carries diffs into live state, stamped with owner. It must be
	// atomic per call: a failure midway leaves the previous state
	// untouched.
	Apply(ctx context.Context, owner Owner, diffs []Diff) error
}

// Waiter is an optional driver capability: block until the objects it
// was asked about are ready. The engine type-asserts Driver to Waiter
// and only waits when ExecutorOptions.Wait is set, so drivers without
// the capability (fake) keep working unchanged.
type Waiter interface {
	// Wait polls until every object in objs is ready or the wait ends.
	// timeout <= 0 means no deadline — the caller's context bounds the
	// wait. It returns nil when all objects are ready, an error naming
	// an object that failed or disappeared, ctx.Err() on cancellation,
	// and a timeout error when timeout > 0 elapses first.
	Wait(ctx context.Context, owner Owner, objs []Object, timeout time.Duration) error
}

// ErrForeignOwner reports a desired object whose live instance carries
// ownership for a different flow or node. Plan refuses instead of
// adopting or clobbering it (design §4). Flow and Node are the owners
// found on the live object.
type ErrForeignOwner struct {
	Flow   string
	Node   string
	Object Object
}

func (e *ErrForeignOwner) Error() string {
	id := e.Object.Kind + "/" + e.Object.Name
	if e.Object.Namespace != "" {
		id = e.Object.Namespace + "/" + id
	}
	return fmt.Sprintf("%s is owned by flow %q node %q; refusing to manage it from another owner",
		id, e.Flow, e.Node)
}

// ErrConflict reports an SSA field conflict: another field manager owns
// fields this apply tried to set and force was not enabled. Managers are
// parsed from the API status. Never returned for optimistic-concurrency
// (resourceVersion) conflicts — those are not field-manager disputes.
type ErrConflict struct {
	Object   Object
	Managers []string
}

func (e *ErrConflict) Error() string {
	return fmt.Sprintf("%s/%s: fields owned by %s; re-run with --force-conflicts to take ownership",
		e.Object.Kind, e.Object.Name, strings.Join(e.Managers, ", "))
}

// ErrClusterScoped reports a cluster-scoped object passed to a driver
// managing one namespace. Phase 3 rejects them explicitly; handling them
// needs flow-level inventory (Phase 4). G/V/K are driver-local strings —
// this leaf package never imports discovery or schema types.
type ErrClusterScoped struct {
	Group   string
	Version string
	Kind    string
}

func (e *ErrClusterScoped) Error() string {
	gv := e.Version
	if e.Group != "" {
		gv = e.Group + "/" + e.Version
	}
	return fmt.Sprintf("%s %s is cluster-scoped; only namespace-scoped objects can be managed", gv, e.Kind)
}

// Canonical returns the deterministic JSON form of an object's Data.
// It is the single normalization used for both hashing and diffing
// (design rules 3 and 7): sorted keys via encoding/json's map handling,
// numbers normalized so 1 and 1.0 are identical.
func Canonical(data map[string]any) ([]byte, error) {
	norm, err := Normalize(data)
	if err != nil {
		return nil, err
	}
	// json.Marshal sorts map keys, and Normalize already made numbers and
	// nested values canonical, so this is stable across runs.
	b, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("canonical encoding: %w", err)
	}
	return b, nil
}

// Normalize deep-copies data, canonicalizing every number (integral floats
// become ints) and recursing into maps and slices. The result hashes and
// compares equal regardless of the input's map order or numeric spelling.
func Normalize(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			nv, err := Normalize(t[k])
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			nv, err := Normalize(e)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	case int:
		return t, nil
	case int8:
		return int(t), nil
	case int16:
		return int(t), nil
	case int32:
		return int(t), nil
	case int64:
		return int(t), nil
	case uint:
		return int(t), nil
	case uint8:
		return int(t), nil
	case uint16:
		return int(t), nil
	case uint32:
		return int(t), nil
	case uint64:
		return int(t), nil
	case float32:
		return canonFloat(float64(t)), nil
	case float64:
		return canonFloat(t), nil
	case bool, string, nil:
		return t, nil
	default:
		return nil, fmt.Errorf("unsupported value of type %T in object data", v)
	}
}

// canonFloat folds an integral float into an int so 1 and 1.0 are equal.
// Non-integral floats pass through; json.Marshal formats them canonically.
func canonFloat(f float64) any {
	if f == float64(int64(f)) && f >= -1e15 && f <= 1e15 {
		return int64(f)
	}
	return f
}

// Equal reports whether two objects' data are canonically identical.
func Equal(a, b map[string]any) bool {
	ca, err1 := Canonical(a)
	cb, err2 := Canonical(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ca) == string(cb)
}
