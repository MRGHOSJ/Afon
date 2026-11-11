package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

// ImageRef is the Phase 2 stand-in for a real image resolution: it "pulls"
// nothing and produces a deterministic fake digest from the ref string, so
// the cache, content-addressing, and output-substitution paths all work
// without a registry.
type ImageRef struct{}

// Type implements NodeType.
func (ImageRef) Type() string { return "image-ref" }

// TypeVersion implements NodeType. Bump when Run's output shape changes.
func (ImageRef) TypeVersion() string { return "1" }

// Kind implements NodeType.
func (ImageRef) Kind() Kind { return KindAction }

// Validate implements NodeType: config must be {ref: <non-empty string>}.
// A whole-string reference marker is also accepted (the value arrives
// resolved by the time Run sees it).
func (ImageRef) Validate(cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a mapping")
	}
	ref, present := m["ref"]
	if !present {
		return fmt.Errorf("missing required field %q", "ref")
	}
	switch v := ref.(type) {
	case string:
		if v == "" {
			return fmt.Errorf("ref must not be empty")
		}
	case Reference, Template:
		return nil
	default:
		return fmt.Errorf("ref must be a string, got %T", ref)
	}
	if len(m) > 1 {
		return fmt.Errorf("unknown fields in config")
	}
	return nil
}

// DeclaredOutputs implements NodeType.
func (ImageRef) DeclaredOutputs() []string { return []string{"ref", "digest"} }

// Run implements ActionType: digest = sha256("image-ref:" + ref).
func (ImageRef) Run(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config must be a mapping")
	}
	ref, ok := m["ref"].(string)
	if !ok || ref == "" {
		return nil, fmt.Errorf("resolved ref must be a non-empty string, got %T", m["ref"])
	}
	sum := sha256.Sum256([]byte("image-ref:" + ref))
	return Outputs{
		"ref":    KnownValue(ref),
		"digest": KnownValue("sha256:" + hex.EncodeToString(sum[:])),
	}, nil
}

// FakeObject is the Phase 2 resource node: it declares one object the fake
// driver should hold, exercising reference substitution, diffing
// (create/update/none), and the cache and plan paths together.
type FakeObject struct{}

// Type implements NodeType.
func (FakeObject) Type() string { return "fake-object" }

// TypeVersion implements NodeType.
func (FakeObject) TypeVersion() string { return "1" }

// Kind implements NodeType.
func (FakeObject) Kind() Kind { return KindResource }

// Validate implements NodeType: {kind, name} non-empty strings, optional
// namespace string, optional data mapping. Field values may be reference
// markers, which resolve before Desired runs.
func (FakeObject) Validate(cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a mapping")
	}
	if err := requireStringish(m, "kind"); err != nil {
		return err
	}
	if err := requireStringish(m, "name"); err != nil {
		return err
	}
	if v, present := m["namespace"]; present {
		if _, isStr := v.(string); !isStr && !isRef(v) {
			return fmt.Errorf("namespace must be a string, got %T", v)
		}
	}
	if v, present := m["data"]; present {
		if _, isMap := v.(map[string]any); !isMap && !isRef(v) {
			return fmt.Errorf("data must be a mapping, got %T", v)
		}
	}
	for k := range m {
		switch k {
		case "kind", "name", "namespace", "data":
		default:
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

// DeclaredOutputs implements NodeType: resource nodes produce no outputs.
func (FakeObject) DeclaredOutputs() []string { return nil }

// Desired implements ResourceType: one driver.Object from the config.
// By the time it runs, every reference in cfg has been resolved by the
// engine, so all values are concrete.
func (FakeObject) Desired(ctx context.Context, cfg any, in Inputs) ([]driver.Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config must be a mapping")
	}
	kind, ok := m["kind"].(string)
	if !ok || kind == "" {
		return nil, fmt.Errorf("resolved kind must be a non-empty string, got %T", m["kind"])
	}
	name, ok := m["name"].(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("resolved name must be a non-empty string, got %T", m["name"])
	}
	obj := driver.Object{
		Group:   "afon.dev",
		Version: "v1",
		Kind:    kind,
		Name:    name,
	}
	if ns, ok := m["namespace"].(string); ok {
		obj.Namespace = ns
	}
	if data, present := m["data"]; present {
		if data == nil {
			data = map[string]any{}
		}
		d, ok := data.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resolved data must be a mapping, got %T", data)
		}
		obj.Data = d
	}
	return []driver.Object{obj}, nil
}

func requireStringish(m map[string]any, key string) error {
	v, present := m[key]
	if !present {
		return fmt.Errorf("missing required field %q", key)
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return fmt.Errorf("%s must not be empty", key)
		}
		return nil
	}
	if isRef(v) {
		return nil
	}
	return fmt.Errorf("%s must be a string, got %T", key, v)
}

func isRef(v any) bool {
	switch v.(type) {
	case Reference, Template:
		return true
	}
	return false
}
