package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"go.yaml.in/yaml/v3"
)

// K8sManifests renders plain-Kubernetes YAML files into one canonical
// multi-document stream for k8s-apply. Plain files only — no Kustomize,
// no overlays — and the rendering is deterministic: path patterns are
// non-recursive globs resolved relative to the flow file's directory,
// matched files are sorted, every document is re-marshaled with sorted
// keys, and the target namespace is injected into documents that do not
// declare one (a declared namespace is left alone; the driver rejects
// one that disagrees with the target rather than rewriting it).
type K8sManifests struct{}

// Type implements NodeType.
func (K8sManifests) Type() string { return "k8s-manifests" }

// TypeVersion implements NodeType. Bump when Run's output shape changes.
func (K8sManifests) TypeVersion() string { return "1" }

// Kind implements NodeType.
func (K8sManifests) Kind() Kind { return KindAction }

// Validate implements NodeType: config must be {paths: [pattern, ...],
// images?: {containerName: image}}. Pattern and image values may be
// reference markers, which resolve before Run sees them.
func (K8sManifests) Validate(cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a mapping")
	}
	rawPaths, present := m["paths"]
	if !present {
		return fmt.Errorf("missing required field %q", "paths")
	}
	list, ok := rawPaths.([]any)
	if !ok || len(list) == 0 {
		return fmt.Errorf("paths must be a non-empty list, got %T", rawPaths)
	}
	for i, v := range list {
		if err := requireStringishValue(v, fmt.Sprintf("paths[%d]", i)); err != nil {
			return err
		}
	}
	if rawImages, present := m["images"]; present && rawImages != nil {
		images, ok := rawImages.(map[string]any)
		if !ok {
			return fmt.Errorf("images must be a mapping, got %T", rawImages)
		}
		for name, v := range images {
			if name == "" {
				return fmt.Errorf("images keys must not be empty")
			}
			if err := requireStringishValue(v, fmt.Sprintf("images[%q]", name)); err != nil {
				return err
			}
		}
	}
	for k := range m {
		switch k {
		case "paths", "images":
		default:
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

// DeclaredOutputs implements NodeType.
func (K8sManifests) DeclaredOutputs() []string { return []string{"rendered"} }

// Run implements ActionType: expand the path patterns, render every
// matched file in sorted order, and emit the joined stream.
func (K8sManifests) Run(ctx context.Context, cfg any, in Inputs) (Outputs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths, images, err := k8sManifestConfig(cfg)
	if err != nil {
		return nil, err
	}
	files, err := expandManifestPaths(paths, in)
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(in.FlowFile)

	var docs []map[string]any
	var empty []string
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("k8s-manifests: reading %s: %w", rel, err)
		}
		fileDocs, err := decodeYAMLDocs(b)
		if err != nil {
			return nil, fmt.Errorf("k8s-manifests: %s: %w", rel, err)
		}
		if len(fileDocs) == 0 {
			empty = append(empty, rel)
			continue
		}
		for i, doc := range fileDocs {
			if err := injectNamespace(doc, in.Target.Namespace); err != nil {
				return nil, fmt.Errorf("k8s-manifests: %s document #%d: %w", rel, i+1, err)
			}
		}
		docs = append(docs, fileDocs...)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("k8s-manifests: no documents found in %s", strings.Join(empty, ", "))
	}
	if err := applyImageOverrides(docs, images); err != nil {
		return nil, err
	}

	var sb strings.Builder
	for i, doc := range docs {
		if i > 0 {
			sb.WriteString("---\n")
		}
		b, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("k8s-manifests: canonical encoding: %w", err)
		}
		sb.Write(b)
	}
	return Outputs{"rendered": KnownValue(sb.String())}, nil
}

// CacheInputs contributes the external state the config cannot hold:
// the expanded path list (a file appearing in or vanishing from a glob
// changes the key) and each file's content hash (an edit changes it),
// plus the target, because the same files render differently per
// namespace. The images map is config and is already keyed.
func (K8sManifests) CacheInputs(cfg any, in Inputs) (string, error) {
	paths, _, err := k8sManifestConfig(cfg)
	if err != nil {
		return "", err
	}
	files, err := expandManifestPaths(paths, in)
	if err != nil {
		return "", err
	}
	base := filepath.Dir(in.FlowFile)

	var sb strings.Builder
	fmt.Fprintf(&sb, "ns=%s ctx=%s\n", in.Target.Namespace, in.Target.Context)
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("k8s-manifests: reading %s: %w", rel, err)
		}
		sum := sha256.Sum256(b)
		fmt.Fprintf(&sb, "%s %s\n", rel, hex.EncodeToString(sum[:]))
	}
	return sb.String(), nil
}

// k8sManifestConfig extracts the resolved config: path patterns and
// container-name → image overrides. By the time this runs (plan key or
// apply) every reference has resolved to a concrete string.
func k8sManifestConfig(cfg any) ([]string, map[string]string, error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("k8s-manifests: config must be a mapping")
	}
	rawPaths, ok := m["paths"].([]any)
	if !ok {
		return nil, nil, fmt.Errorf("k8s-manifests: paths must be a list, got %T", m["paths"])
	}
	paths := make([]string, 0, len(rawPaths))
	for i, v := range rawPaths {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, nil, fmt.Errorf("k8s-manifests: paths[%d] must be a resolved non-empty string, got %T", i, v)
		}
		paths = append(paths, s)
	}
	images := map[string]string{}
	if rawImages, present := m["images"]; present && rawImages != nil {
		im, ok := rawImages.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("k8s-manifests: images must be a mapping, got %T", rawImages)
		}
		for name, v := range im {
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, nil, fmt.Errorf("k8s-manifests: images[%q] must be a resolved non-empty string, got %T", name, v)
			}
			images[name] = s
		}
	}
	return paths, images, nil
}

// expandManifestPaths resolves each pattern against the flow file's
// directory (non-recursive globs). Every pattern must match at least
// one file — a typo must fail loudly instead of rendering nothing —
// and matches are deduped and sorted so the rendered stream and the
// cache key always agree. Returned paths are slash-separated and
// relative to the flow file's directory.
func expandManifestPaths(patterns []string, in Inputs) ([]string, error) {
	if in.FlowFile == "" {
		return nil, fmt.Errorf("k8s-manifests: cannot resolve paths without a flow file")
	}
	base := filepath.Dir(in.FlowFile)
	seen := map[string]bool{}
	var out []string
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(base, pat))
		if err != nil {
			return nil, fmt.Errorf("k8s-manifests: bad path pattern %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("k8s-manifests: no files match %q (relative to %s)", pat, base)
		}
		for _, abs := range matches {
			rel, err := filepath.Rel(base, abs)
			if err != nil {
				return nil, fmt.Errorf("k8s-manifests: path %q: %w", pat, err)
			}
			key := filepath.ToSlash(rel)
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// decodeYAMLDocs splits a multi-document YAML stream, skipping empty
// documents. Errors name the document index for the caller to wrap
// with its file (or node) context.
func decodeYAMLDocs(b []byte) ([]map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []map[string]any
	for i := 0; ; i++ {
		var doc any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("document #%d: %w", i+1, err)
		}
		if doc == nil {
			continue
		}
		m, ok := doc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("document #%d is not a mapping (got %T)", i+1, doc)
		}
		out = append(out, m)
	}
	return out, nil
}

// injectNamespace fills metadata.namespace with the flow target when
// the document does not declare one. A declared namespace is never
// rewritten: the driver rejects a namespace that disagrees with its
// target, so a manifest pinned elsewhere fails loudly at plan time
// instead of silently landing somewhere else.
func injectNamespace(doc map[string]any, ns string) error {
	if ns == "" {
		return nil
	}
	raw, present := doc["metadata"]
	if !present || raw == nil {
		doc["metadata"] = map[string]any{"namespace": ns}
		return nil
	}
	meta, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("metadata must be a mapping, got %T", raw)
	}
	if v, present := meta["namespace"]; !present || v == nil || v == "" {
		meta["namespace"] = ns
	}
	return nil
}

// applyImageOverrides rewrites container images by name across every
// rendered document. The walk is structural — any map key named
// "containers" or "initContainers" holding a list — so pod templates
// at any depth (Deployment, CronJob's jobTemplate, CRDs) work without
// a kind list. A name that matches no container is an error listing
// what exists, so a typo fails loudly instead of silently doing
// nothing.
func applyImageOverrides(docs []map[string]any, images map[string]string) error {
	if len(images) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, doc := range docs {
		walkContainers(doc, func(c map[string]any) {
			name, _ := c["name"].(string)
			if name == "" {
				return
			}
			seen[name] = true
			if img, ok := images[name]; ok {
				c["image"] = img
			}
		})
	}
	var missing []string
	for name := range images {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	have := make([]string, 0, len(seen))
	for name := range seen {
		have = append(have, name)
	}
	sort.Strings(have)
	haveStr := strings.Join(have, ", ")
	if haveStr == "" {
		haveStr = "none"
	}
	quoted := make([]string, len(missing))
	for i, name := range missing {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return fmt.Errorf("k8s-manifests: image override: no container named %s (have: %s)",
		strings.Join(quoted, ", "), haveStr)
}

// walkContainers calls fn for every map found under a "containers" or
// "initContainers" list, at any depth.
func walkContainers(v any, fn func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		for key, child := range t {
			if key == "containers" || key == "initContainers" {
				if list, ok := child.([]any); ok {
					for _, elem := range list {
						if m, ok := elem.(map[string]any); ok {
							fn(m)
						}
					}
				}
			}
			walkContainers(child, fn)
		}
	case []any:
		for _, elem := range t {
			walkContainers(elem, fn)
		}
	}
}

// K8sApply turns a rendered multi-document YAML stream into the
// desired objects for the k8s driver. Identity comes from each
// document's apiVersion/kind/metadata.name; Data carries the whole
// document so the driver can diff and apply it verbatim. Resource
// nodes never consult the cache in Phase 3.
type K8sApply struct{}

// Type implements NodeType.
func (K8sApply) Type() string { return "k8s-apply" }

// TypeVersion implements NodeType. Bump when Desired's object shape changes.
func (K8sApply) TypeVersion() string { return "1" }

// Kind implements NodeType.
func (K8sApply) Kind() Kind { return KindResource }

// Validate implements NodeType: config must be {manifests: <string or
// whole-string reference>}.
func (K8sApply) Validate(cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a mapping")
	}
	v, present := m["manifests"]
	if !present {
		return fmt.Errorf("missing required field %q", "manifests")
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return fmt.Errorf("manifests must not be empty")
		}
	case Reference, Template:
	default:
		return fmt.Errorf("manifests must be a string, got %T", v)
	}
	for k := range m {
		if k != "manifests" {
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

// DeclaredOutputs implements NodeType: resource nodes produce no outputs.
func (K8sApply) DeclaredOutputs() []string { return nil }

// Desired implements ResourceType: parse the rendered stream into one
// driver.Object per document.
func (K8sApply) Desired(ctx context.Context, cfg any, in Inputs) ([]driver.Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("k8s-apply: config must be a mapping")
	}
	raw, ok := m["manifests"].(string)
	if !ok {
		return nil, fmt.Errorf("k8s-apply: manifests must be a resolved string, got %T", m["manifests"])
	}
	docs, err := decodeYAMLDocs([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("k8s-apply: %w", err)
	}
	objs := make([]driver.Object, 0, len(docs))
	for i, doc := range docs {
		obj, err := objectFromDoc(doc)
		if err != nil {
			return nil, fmt.Errorf("k8s-apply: document #%d: %w", i+1, err)
		}
		objs = append(objs, obj)
	}
	if len(objs) == 0 {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("k8s-apply: no objects found in manifests (input is empty)")
		}
		return nil, fmt.Errorf("k8s-apply: no objects found in manifests (no mapping documents)")
	}
	return objs, nil
}

// objectFromDoc lifts one document into a driver.Object: identity from
// apiVersion/kind/metadata.name, namespace when declared, Data as the
// whole document.
func objectFromDoc(doc map[string]any) (driver.Object, error) {
	apiVersion, err := docString(doc, "apiVersion")
	if err != nil {
		return driver.Object{}, err
	}
	kind, err := docString(doc, "kind")
	if err != nil {
		return driver.Object{}, err
	}
	var meta map[string]any
	if raw, present := doc["metadata"]; present && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return driver.Object{}, fmt.Errorf("metadata must be a mapping, got %T", raw)
		}
		meta = m
	}
	name, err := docString(meta, "name")
	if err != nil {
		return driver.Object{}, fmt.Errorf("metadata.%w", err)
	}
	group, version := "", apiVersion
	if i := strings.Index(apiVersion, "/"); i >= 0 {
		group, version = apiVersion[:i], apiVersion[i+1:]
	}
	obj := driver.Object{
		Group: group, Version: version, Kind: kind, Name: name, Data: doc,
	}
	if ns, ok := meta["namespace"].(string); ok && ns != "" {
		obj.Namespace = ns
	}
	return obj, nil
}

// docString requires a non-empty string field, naming the field the
// way the caller wants it shown (bare for top-level fields, prefixed
// by the caller for nested ones).
func docString(m map[string]any, key string) (string, error) {
	v, present := m[key]
	if !present || v == nil || v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", key, v)
	}
	return s, nil
}

// requireStringishValue accepts a non-empty string or an unresolved
// reference marker (Validate runs before resolution).
func requireStringishValue(v any, what string) error {
	switch t := v.(type) {
	case string:
		if t == "" {
			return fmt.Errorf("%s must not be empty", what)
		}
		return nil
	case Reference, Template:
		return nil
	default:
		return fmt.Errorf("%s must be a string, got %T", what, v)
	}
}
