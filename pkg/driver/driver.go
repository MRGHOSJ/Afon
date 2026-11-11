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
)

// Diff is one object's planned change.
type Diff struct {
	Object Object `json:"object"`
	Op     Op     `json:"op"`
}

// Driver plans and applies resource changes. Plan is strictly read-only.
type Driver interface {
	// Plan diffs desired objects against live state. It must not mutate
	// anything, including the driver's own persisted state.
	Plan(ctx context.Context, desired []Object) ([]Diff, error)
	// Apply carries diffs into live state. It must be atomic per call: a
	// failure midway leaves the previous state untouched.
	Apply(ctx context.Context, diffs []Diff) error
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
