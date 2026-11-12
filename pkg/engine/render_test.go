package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matNode is a test-local materialized action: config {value, out?},
// one output "rendered". Run emits value + "\n" — byte-identical to
// the normalized body written to the file, so render-time chains and
// plan/apply-time chains resolve the same bytes. Stands in for
// AppManifests until 3.6b lands.
type matNode struct{}

func (matNode) Type() string        { return "test-mat" }
func (matNode) TypeVersion() string { return "1" }
func (matNode) Kind() Kind          { return KindAction }

func (matNode) Validate(cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a mapping")
	}
	v, present := m["value"]
	if !present {
		return fmt.Errorf("missing required field %q", "value")
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return fmt.Errorf("value must not be empty")
		}
	case Reference, Template:
	default:
		return fmt.Errorf("value must be a string, got %T", v)
	}
	if raw, present := m["out"]; present {
		s, ok := raw.(string)
		if !ok || s == "" {
			return fmt.Errorf("out must be a non-empty string, got %T", raw)
		}
	}
	for k := range m {
		switch k {
		case "value", "out":
		default:
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

func (matNode) DeclaredOutputs() []string { return []string{"rendered"} }

func (matNode) Run(_ context.Context, cfg any, _ Inputs) (Outputs, error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config must be a mapping")
	}
	v, ok := m["value"].(string)
	if !ok || v == "" {
		return nil, fmt.Errorf("value must be a resolved non-empty string, got %T", m["value"])
	}
	return Outputs{"rendered": KnownValue(v + "\n")}, nil
}

func (matNode) MaterializedPath(cfg any, in Inputs) (string, error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return "", fmt.Errorf("config must be a mapping")
	}
	raw, present := m["out"]
	if !present || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("out must be a resolved non-empty string, got %T", raw)
	}
	if filepath.IsAbs(s) {
		return filepath.ToSlash(s), nil
	}
	if in.FlowFile == "" {
		return "", fmt.Errorf("cannot resolve out without a flow file")
	}
	return filepath.ToSlash(filepath.Join(filepath.Dir(in.FlowFile), s)), nil
}

func matRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, nt := range []NodeType{matNode{}, FakeObject{}} {
		if err := reg.Register(nt); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// compileMat compiles src and anchors file-relative paths at a fresh
// temp dir: Compile keeps "flow.yaml" as the name, and out: resolves
// against cf.File's directory — tests must never write into the
// package directory.
func compileMat(t *testing.T, src string) *CompiledFlow {
	t.Helper()
	cf, err := compileWith(t, src, matRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	cf.File = filepath.Join(t.TempDir(), "flow.yaml")
	return cf
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const matFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: m
      type: test-mat
      config:
        value: hello
        out: gen/out.yaml
`

func TestRenderActions_WritesFile(t *testing.T) {
	cf := compileMat(t, matFlow)
	res, err := RenderActions(context.Background(), cf, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("files = %+v, want one", res.Files)
	}
	f := res.Files[0]
	if f.NodeID != "m" || f.Status != FileWritten {
		t.Errorf("outcome = %+v", f)
	}
	wantPath := filepath.ToSlash(filepath.Join(filepath.Dir(cf.File), "gen", "out.yaml"))
	if f.Path != wantPath {
		t.Errorf("path = %q, want %q", f.Path, wantPath)
	}

	meta, body, ok := parseProvenance([]byte(readFile(t, f.Path)))
	if !ok {
		t.Fatal("written file has no valid provenance header")
	}
	if string(body) != "hello\n" {
		t.Errorf("body = %q, want %q", body, "hello\n")
	}
	if meta.Node != "m" || meta.Flow != cf.File {
		t.Errorf("header flow/node = %q/%q", meta.Flow, meta.Node)
	}
	cn, _ := cf.Node("m")
	wantCfg, err := configHash(cn.Config)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ConfigHash != wantCfg {
		t.Errorf("config hash = %q, want %q", meta.ConfigHash, wantCfg)
	}
	if meta.BodyHash != hashHex([]byte("hello\n")) {
		t.Errorf("body hash = %q", meta.BodyHash)
	}

	// Run outputs stay header-free: the header is provenance, not content.
	if got := res.Outputs["m"]["rendered"]; !got.Known || got.Data != "hello\n" {
		t.Errorf("outputs = %+v, want body only", got)
	}
}

func TestRenderActions_Idempotent(t *testing.T) {
	cf := compileMat(t, matFlow)
	if _, err := RenderActions(context.Background(), cf, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.ToSlash(filepath.Join(filepath.Dir(cf.File), "gen", "out.yaml"))
	first := readFile(t, path)

	res, err := RenderActions(context.Background(), cf, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Status != FileUnchanged {
		t.Fatalf("second render = %+v, want unchanged", res.Files)
	}
	if got := readFile(t, path); got != first {
		t.Errorf("file changed on re-render:\n%s", got)
	}
}

func TestRenderActions_RefusesEdit(t *testing.T) {
	cf := compileMat(t, matFlow)
	if _, err := RenderActions(context.Background(), cf, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.ToSlash(filepath.Join(filepath.Dir(cf.File), "gen", "out.yaml"))

	edited := strings.Replace(readFile(t, path), "hello", "goodbye", 1)
	if err := os.WriteFile(filepath.FromSlash(path), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := RenderActions(context.Background(), cf, RenderOptions{})
	if err == nil {
		t.Fatal("render over an edited file must be refused")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error must name the path and --force: %v", err)
	}
	if got := readFile(t, path); got != edited {
		t.Error("refused render must not touch the file")
	}

	res, err := RenderActions(context.Background(), cf, RenderOptions{Force: true})
	if err != nil {
		t.Fatalf("force render: %v", err)
	}
	if res.Files[0].Status != FileWritten {
		t.Errorf("force outcome = %+v", res.Files[0])
	}
	if got := readFile(t, path); !strings.Contains(got, "hello") || strings.Contains(got, "goodbye") {
		t.Errorf("force did not regenerate:\n%s", got)
	}
}

func TestRenderActions_Check(t *testing.T) {
	cf := compileMat(t, matFlow)
	path := filepath.ToSlash(filepath.Join(filepath.Dir(cf.File), "gen", "out.yaml"))

	// Missing file: reported, never created by --check.
	res, err := RenderActions(context.Background(), cf, RenderOptions{Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Status != FileMissing {
		t.Errorf("missing check = %+v", res.Files[0])
	}
	if _, err := os.Stat(filepath.FromSlash(path)); !os.IsNotExist(err) {
		t.Error("--check must not create the file")
	}

	// Up to date after a real render.
	if _, err := RenderActions(context.Background(), cf, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err = RenderActions(context.Background(), cf, RenderOptions{Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Status != FileUpToDate || res.Files[0].Diff != "" {
		t.Errorf("up-to-date check = %+v", res.Files[0])
	}

	// Edited: reported with a diff, file untouched.
	edited := strings.Replace(readFile(t, path), "hello", "goodbye", 1)
	if err := os.WriteFile(filepath.FromSlash(path), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = RenderActions(context.Background(), cf, RenderOptions{Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Status != FileEdited || !strings.Contains(res.Files[0].Diff, "first difference") {
		t.Errorf("edited check = %+v", res.Files[0])
	}
	if got := readFile(t, path); got != edited {
		t.Error("--check must not modify an edited file")
	}

	// Restore the file, then change the flow config: stale, not edited.
	if _, err := RenderActions(context.Background(), cf, RenderOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	staleFlow := strings.Replace(matFlow, "value: hello", "value: hello2", 1)
	staleCF := compileMat(t, staleFlow)
	staleCF.File = cf.File // same path on disk: only the config differs
	res, err = RenderActions(context.Background(), staleCF, RenderOptions{Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Status != FileStale {
		t.Errorf("stale check = %+v", res.Files[0])
	}
}

func TestRenderActions_NoMaterialized(t *testing.T) {
	flow := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: m
      type: test-mat
      config: { value: hello }
`
	cf := compileMat(t, flow)
	_, err := RenderActions(context.Background(), cf, RenderOptions{})
	if err == nil || !strings.Contains(err.Error(), "no materialized nodes") {
		t.Fatalf("err = %v, want no-materialized-nodes error", err)
	}

	// --node on the ephemeral node still works: run and print, no file.
	res, err := RenderActions(context.Background(), cf, RenderOptions{Only: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 0 {
		t.Errorf("ephemeral --node wrote files: %+v", res.Files)
	}
	if got := res.Outputs["m"]["rendered"]; got.Data != "hello\n" {
		t.Errorf("outputs = %+v", got)
	}
}

func TestRenderActions_OnlyRestrictsWrites(t *testing.T) {
	both := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: a
      type: test-mat
      config: { value: alpha, out: gen/a.yaml }
    - id: b
      type: test-mat
      config: { value: "${a.rendered}beta", out: gen/b.yaml }
    - id: obj
      type: fake-object
      config: { kind: ConfigMap, name: demo }
`
	cf := compileMat(t, both)
	dir := filepath.Dir(cf.File)

	// --node b: runs a for resolution, writes only b.
	res, err := RenderActions(context.Background(), cf, RenderOptions{Only: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].NodeID != "b" {
		t.Fatalf("files = %+v, want only b", res.Files)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen", "a.yaml")); !os.IsNotExist(err) {
		t.Error("--node must not write upstream materialized files")
	}
	if _, err := os.Stat(filepath.Join(dir, "gen", "b.yaml")); err != nil {
		t.Fatalf("--node must write its own file: %v", err)
	}
	if _, ok := res.Outputs["a"]; !ok {
		t.Error("upstream action must still run for reference resolution")
	}
	// b's body comes from a's Run output: "alpha\n" + "beta" + "\n".
	if !strings.Contains(readFile(t, filepath.ToSlash(filepath.Join(dir, "gen", "b.yaml"))), "alpha") {
		t.Error("b's file must incorporate a's output")
	}

	// Unknown node / resource node are errors.
	if _, err := RenderActions(context.Background(), cf, RenderOptions{Only: "nope"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown --node err = %v", err)
	}
	if _, err := RenderActions(context.Background(), cf, RenderOptions{Only: "obj"}); err == nil || !strings.Contains(err.Error(), "action nodes only") {
		t.Errorf("resource --node err = %v", err)
	}
}

func TestRenderActions_OutInsteadOfConfigured(t *testing.T) {
	cf := compileMat(t, matFlow)
	dir := filepath.Dir(cf.File)
	outPath := filepath.ToSlash(filepath.Join(dir, "adhoc.yaml"))

	if _, err := RenderActions(context.Background(), cf, RenderOptions{Out: outPath}); err == nil {
		t.Error("--out without --node must fail")
	}

	res, err := RenderActions(context.Background(), cf, RenderOptions{Only: "m", Out: outPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != outPath || res.Files[0].Status != FileWritten {
		t.Fatalf("files = %+v", res.Files)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen", "out.yaml")); !os.IsNotExist(err) {
		t.Error("--out must write there INSTEAD of the configured out:")
	}
	meta, body, ok := parseProvenance([]byte(readFile(t, outPath)))
	if !ok || string(body) != "hello\n" || meta.Node != "m" {
		t.Errorf("ad-hoc file = ok:%v body:%q meta:%+v", ok, body, meta)
	}
}

func TestRenderActions_ChainedAndSkipsResources(t *testing.T) {
	flow := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: seed
      type: test-mat
      config: { value: seedvalue }
    - id: m
      type: test-mat
      config: { value: "${seed.rendered}+", out: gen/out.yaml }
    - id: obj
      type: fake-object
      config: { kind: ConfigMap, name: demo }
`
	cf := compileMat(t, flow)
	res, err := RenderActions(context.Background(), cf, RenderOptions{Mode: RenderAll})
	if err != nil {
		t.Fatal(err)
	}
	// Resource node never runs in render.
	if _, ok := res.Outputs["obj"]; ok {
		t.Error("resource node must be skipped by render")
	}
	// Default mode (RenderMaterialized): seed is a materialized node's
	// ancestor so it runs; only m's file is written.
	res, err = RenderActions(context.Background(), cf, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["seed"]; !ok {
		t.Error("ancestor action must run in default mode")
	}
	if len(res.Files) != 1 || res.Files[0].NodeID != "m" {
		t.Errorf("files = %+v, want only m", res.Files)
	}
	_, body, _ := parseProvenance([]byte(readFile(t, res.Files[0].Path)))
	if !strings.HasPrefix(string(body), "seedvalue\n+") {
		t.Errorf("chained body = %q, want seed output incorporated", body)
	}
}
