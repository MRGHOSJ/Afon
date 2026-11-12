package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// AppManifests generates a Deployment (and a Service when a port is
// set) from a small app spec — the "write a flow, not Kubernetes YAML"
// path. Output is deterministic: canonical yaml.Marshal with sorted
// keys, env entries sorted by name, replicas always emitted. The
// namespace comes from the flow target (the generator owns it, so it
// can never disagree with the driver's). Set out: to materialize the
// rendered manifest to a file for afon render → plan → apply; leave it
// unset for the ephemeral in-memory chain.
type AppManifests struct{}

// Type implements NodeType.
func (AppManifests) Type() string { return "app-manifests" }

// TypeVersion implements NodeType. Bump when Run's output shape changes.
func (AppManifests) TypeVersion() string { return "1" }

// Kind implements NodeType.
func (AppManifests) Kind() Kind { return KindAction }

// dns1123Label is the Kubernetes name grammar: lowercase
// alphanumerics and '-', must start and end alphanumeric, ≤63 chars.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate implements NodeType: {name, image} required, {replicas,
// port, env, out} optional, everything else rejected. name, replicas,
// port, and out must be literals — materialization and the emitted
// object identity have to be predictable; image and env values accept
// reference markers, which resolve before Run sees them.
func (AppManifests) Validate(cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a mapping")
	}

	rawName, present := m["name"]
	if !present {
		return fmt.Errorf("missing required field %q", "name")
	}
	name, ok := rawName.(string)
	if !ok {
		return fmt.Errorf("name must be a string, got %T", rawName)
	}
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if len(name) > 63 || !dns1123Label.MatchString(name) {
		return fmt.Errorf("name %q must be a DNS-1123 label (lowercase alphanumerics and '-', must start and end alphanumeric, ≤63 chars)", name)
	}

	rawImage, present := m["image"]
	if !present {
		return fmt.Errorf("missing required field %q", "image")
	}
	switch t := rawImage.(type) {
	case string:
		if t == "" {
			return fmt.Errorf("image must not be empty")
		}
	case Reference, Template:
	default:
		return fmt.Errorf("image must be a string, got %T", rawImage)
	}

	if raw, present := m["replicas"]; present {
		r, ok := raw.(int)
		if !ok {
			return fmt.Errorf("replicas must be an integer, got %T", raw)
		}
		if r < 0 {
			return fmt.Errorf("replicas must be >= 0, got %d", r)
		}
	}

	if raw, present := m["port"]; present {
		p, ok := raw.(int)
		if !ok {
			return fmt.Errorf("port must be an integer, got %T", raw)
		}
		if p < 1 || p > 65535 {
			return fmt.Errorf("port must be between 1 and 65535, got %d", p)
		}
	}

	if raw, present := m["env"]; present && raw != nil {
		em, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("env must be a mapping, got %T", raw)
		}
		for k, v := range em {
			if k == "" {
				return fmt.Errorf("env keys must not be empty")
			}
			switch v.(type) {
			case string:
			case Reference, Template:
			default:
				return fmt.Errorf("env[%q] must be a string, got %T", k, v)
			}
		}
	}

	if raw, present := m["out"]; present {
		s, ok := raw.(string)
		if !ok || s == "" {
			return fmt.Errorf("out must be a non-empty string, got %T", raw)
		}
	}

	for k := range m {
		switch k {
		case "name", "image", "replicas", "port", "env", "out":
		default:
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

// DeclaredOutputs implements NodeType.
func (AppManifests) DeclaredOutputs() []string { return []string{"rendered"} }

// Run implements ActionType: build the Deployment (+ Service when a
// port is set) and emit the canonical multi-document stream. No
// provenance header here — Run's output is pure YAML; render adds the
// header only when writing a file, so a non-materialized
// app-manifests → k8s-apply chain sees clean bytes.
func (AppManifests) Run(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, image, replicas, port, env, err := appManifestConfig(cfg)
	if err != nil {
		return nil, err
	}
	ns := in.Target.Namespace
	if ns == "" {
		return nil, fmt.Errorf("app-manifests: target namespace is required")
	}

	labels := map[string]any{"app": name}
	container := map[string]any{"name": name, "image": image}
	if port != 0 {
		container["ports"] = []any{map[string]any{"containerPort": port}}
	}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		list := make([]any, 0, len(keys))
		for _, k := range keys {
			list = append(list, map[string]any{"name": k, "value": env[k]})
		}
		container["env"] = list
	}

	deployment := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"containers": []any{container},
				},
			},
		},
	}
	docs := []any{deployment}
	if port != 0 {
		docs = append(docs, map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
				"labels":    labels,
			},
			"spec": map[string]any{
				"selector": labels,
				"ports":    []any{map[string]any{"port": 80, "targetPort": port}},
			},
		})
	}

	var sb strings.Builder
	for i, doc := range docs {
		if i > 0 {
			sb.WriteString("---\n")
		}
		b, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("app-manifests: canonical encoding: %w", err)
		}
		sb.Write(b)
	}
	return Outputs{"rendered": KnownValue(sb.String())}, nil
}

// CacheInputs contributes the state config cannot hold: the same app
// spec renders differently per target, so the namespace and context
// key the cache. The out: path is config and is already keyed.
func (AppManifests) CacheInputs(_ any, in Inputs) (string, error) {
	return fmt.Sprintf("ns=%s ctx=%s\n", in.Target.Namespace, in.Target.Context), nil
}

// MaterializedPath implements MaterializedNode: out: resolved
// relative to the flow file's directory (never the process cwd),
// slash-normalized; absolute paths pass through; out: unset keeps the
// node ephemeral. out is required to be a literal by Validate, so the
// marked config carries the same value the resolved config would.
func (AppManifests) MaterializedPath(cfg any, in Inputs) (string, error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return "", fmt.Errorf("app-manifests: config must be a mapping")
	}
	raw, present := m["out"]
	if !present || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("app-manifests: out must be a resolved non-empty string, got %T", raw)
	}
	if filepath.IsAbs(s) {
		return filepath.ToSlash(s), nil
	}
	if in.FlowFile == "" {
		return "", fmt.Errorf("app-manifests: cannot resolve out without a flow file")
	}
	return filepath.ToSlash(filepath.Join(filepath.Dir(in.FlowFile), s)), nil
}

// appManifestConfig extracts the resolved config with field-named
// errors. By the time Run runs, references have resolved — image and
// env values arrive as concrete strings.
func appManifestConfig(cfg any) (name, image string, replicas, port int, env map[string]string, err error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return "", "", 0, 0, nil, fmt.Errorf("app-manifests: config must be a mapping")
	}
	name, ok = m["name"].(string)
	if !ok || name == "" {
		return "", "", 0, 0, nil, fmt.Errorf("app-manifests: name must be a non-empty string, got %T", m["name"])
	}
	image, ok = m["image"].(string)
	if !ok || image == "" {
		return "", "", 0, 0, nil, fmt.Errorf("app-manifests: image must be a resolved non-empty string, got %T", m["image"])
	}
	replicas = 1
	if raw, present := m["replicas"]; present {
		replicas, ok = raw.(int)
		if !ok {
			return "", "", 0, 0, nil, fmt.Errorf("app-manifests: replicas must be an integer, got %T", raw)
		}
	}
	port = 0
	if raw, present := m["port"]; present {
		port, ok = raw.(int)
		if !ok {
			return "", "", 0, 0, nil, fmt.Errorf("app-manifests: port must be an integer, got %T", raw)
		}
	}
	env = map[string]string{}
	if raw, present := m["env"]; present && raw != nil {
		em, ok := raw.(map[string]any)
		if !ok {
			return "", "", 0, 0, nil, fmt.Errorf("app-manifests: env must be a mapping, got %T", raw)
		}
		for k, v := range em {
			s, ok := v.(string)
			if !ok {
				return "", "", 0, 0, nil, fmt.Errorf("app-manifests: env[%q] must be a resolved string, got %T", k, v)
			}
			env[k] = s
		}
	}
	return name, image, replicas, port, env, nil
}
