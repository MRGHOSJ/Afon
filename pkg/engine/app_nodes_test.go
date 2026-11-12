package engine

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/schema"
	"go.yaml.in/yaml/v3"
)

func TestAppManifests_Validate(t *testing.T) {
	ref := Reference{Node: "seed", Output: "rendered"}
	tmpl := Template{Parts: []any{"registry.example.com/", ref}}
	cases := []struct {
		name string
		cfg  any
		ok   bool
	}{
		{"minimal", map[string]any{"name": "hello", "image": "nginx:1.27-alpine"}, true},
		{"full", map[string]any{
			"name": "hello", "image": "nginx:1.27-alpine", "replicas": 2,
			"port": 8080, "env": map[string]any{"A": "1", "B": ref}, "out": "gen/hello.yaml",
		}, true},
		{"image reference", map[string]any{"name": "hello", "image": ref}, true},
		{"image template", map[string]any{"name": "hello", "image": tmpl}, true},
		{"replicas zero", map[string]any{"name": "hello", "image": "img", "replicas": 0}, true},
		{"env empty value", map[string]any{"name": "hello", "image": "img", "env": map[string]any{"K": ""}}, true},

		{"config not a mapping", []any{"nope"}, false},
		{"missing name", map[string]any{"image": "img"}, false},
		{"empty name", map[string]any{"name": "", "image": "img"}, false},
		{"name as reference", map[string]any{"name": ref, "image": "img"}, false},
		{"name not a string", map[string]any{"name": 42, "image": "img"}, false},
		{"name with underscore", map[string]any{"name": "Hello_World", "image": "img"}, false},
		{"name uppercase", map[string]any{"name": "UPPER", "image": "img"}, false},
		{"name leading dash", map[string]any{"name": "-x", "image": "img"}, false},
		{"name trailing dash", map[string]any{"name": "x-", "image": "img"}, false},
		{"name too long", map[string]any{"name": strings.Repeat("a", 64), "image": "img"}, false},
		{"missing image", map[string]any{"name": "hello"}, false},
		{"empty image", map[string]any{"name": "hello", "image": ""}, false},
		{"image wrong type", map[string]any{"name": "hello", "image": 123}, false},
		{"replicas string", map[string]any{"name": "hello", "image": "img", "replicas": "2"}, false},
		{"replicas negative", map[string]any{"name": "hello", "image": "img", "replicas": -1}, false},
		{"replicas reference", map[string]any{"name": "hello", "image": "img", "replicas": ref}, false},
		{"port zero", map[string]any{"name": "hello", "image": "img", "port": 0}, false},
		{"port too big", map[string]any{"name": "hello", "image": "img", "port": 65536}, false},
		{"port string", map[string]any{"name": "hello", "image": "img", "port": "8080"}, false},
		{"port reference", map[string]any{"name": "hello", "image": "img", "port": ref}, false},
		{"env not a mapping", map[string]any{"name": "hello", "image": "img", "env": []any{"A=1"}}, false},
		{"env empty key", map[string]any{"name": "hello", "image": "img", "env": map[string]any{"": "1"}}, false},
		{"env value wrong type", map[string]any{"name": "hello", "image": "img", "env": map[string]any{"A": 1}}, false},
		{"out empty", map[string]any{"name": "hello", "image": "img", "out": ""}, false},
		{"out reference", map[string]any{"name": "hello", "image": "img", "out": ref}, false},
		{"out wrong type", map[string]any{"name": "hello", "image": "img", "out": 42}, false},
		{"unknown field", map[string]any{"name": "hello", "image": "img", "typo": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AppManifests{}.Validate(tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

// parseDocs splits a canonical multi-document render into raw maps.
func parseDocs(t *testing.T, rendered string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for _, part := range strings.Split(rendered, "\n---\n") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		var d map[string]any
		if err := yaml.Unmarshal([]byte(part), &d); err != nil {
			t.Fatalf("unmarshal doc: %v\n%s", err, part)
		}
		docs = append(docs, d)
	}
	return docs
}

func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	cur := any(m)
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("dig %v: not a mapping at %q (got %T)", path, k, cur)
		}
		cur, ok = mm[k]
		if !ok {
			t.Fatalf("dig %v: missing %q", path, k)
		}
	}
	return cur
}

func TestAppManifests_Run(t *testing.T) {
	ctx := context.Background()
	in := Inputs{Target: schema.Target{Namespace: "hello"}}

	t.Run("full spec with port and env", func(t *testing.T) {
		cfg := map[string]any{
			"name": "hello", "image": "registry.example.com/hello:1.0.0", "replicas": 2,
			"port": 8080, "env": map[string]any{"LOG_LEVEL": "debug", "MODE": "prod"},
		}
		outs, err := AppManifests{}.Run(ctx, cfg, in)
		if err != nil {
			t.Fatal(err)
		}
		rendered, _ := outs["rendered"].Data.(string)
		docs := parseDocs(t, rendered)
		if len(docs) != 2 {
			t.Fatalf("docs = %d, want Deployment + Service", len(docs))
		}
		dep, svc := docs[0], docs[1]
		if dig(t, dep, "kind") != "Deployment" || dig(t, dep, "apiVersion") != "apps/v1" {
			t.Errorf("doc[0] = %v", dep)
		}
		if dig(t, svc, "kind") != "Service" || dig(t, svc, "apiVersion") != "v1" {
			t.Errorf("doc[1] = %v", svc)
		}
		if got := dig(t, dep, "metadata", "namespace"); got != "hello" {
			t.Errorf("namespace = %v", got)
		}
		if got := dig(t, dep, "metadata", "labels", "app"); got != "hello" {
			t.Errorf("metadata label = %v", got)
		}
		if got := dig(t, dep, "spec", "replicas"); got != 2 {
			t.Errorf("replicas = %v, want 2", got)
		}
		if got := dig(t, dep, "spec", "selector", "matchLabels", "app"); got != "hello" {
			t.Errorf("selector = %v", got)
		}
		if got := dig(t, dep, "spec", "template", "metadata", "labels", "app"); got != "hello" {
			t.Errorf("pod label = %v", got)
		}
		ctr, _ := dig(t, dep, "spec", "template", "spec", "containers").([]any)
		if len(ctr) != 1 {
			t.Fatalf("containers = %v", ctr)
		}
		c0, _ := ctr[0].(map[string]any)
		if c0["name"] != "hello" || c0["image"] != "registry.example.com/hello:1.0.0" {
			t.Errorf("container = %v", c0)
		}
		ports, _ := c0["ports"].([]any)
		if len(ports) != 1 {
			t.Fatalf("ports = %v", ports)
		}
		p0, _ := ports[0].(map[string]any)
		if p0["containerPort"] != 8080 {
			t.Errorf("containerPort = %v", p0["containerPort"])
		}
		envList, _ := c0["env"].([]any)
		if len(envList) != 2 {
			t.Fatalf("env = %v", envList)
		}
		e0, _ := envList[0].(map[string]any)
		e1, _ := envList[1].(map[string]any)
		if e0["name"] != "LOG_LEVEL" || e0["value"] != "debug" {
			t.Errorf("env[0] = %v, want sorted-by-name first entry", e0)
		}
		if e1["name"] != "MODE" || e1["value"] != "prod" {
			t.Errorf("env[1] = %v", e1)
		}
		// Service: port 80 targets the container port, same selector/labels.
		if got := dig(t, svc, "spec", "selector", "app"); got != "hello" {
			t.Errorf("service selector = %v", got)
		}
		sp, _ := dig(t, svc, "spec", "ports").([]any)
		if len(sp) != 1 {
			t.Fatalf("service ports = %v", sp)
		}
		sp0, _ := sp[0].(map[string]any)
		if sp0["port"] != 80 || sp0["targetPort"] != 8080 {
			t.Errorf("service port = %v", sp0)
		}
		if got := dig(t, svc, "metadata", "namespace"); got != "hello" {
			t.Errorf("service namespace = %v", got)
		}
	})

	t.Run("no port emits one doc without ports", func(t *testing.T) {
		outs, err := AppManifests{}.Run(ctx, map[string]any{"name": "bare", "image": "img"}, in)
		if err != nil {
			t.Fatal(err)
		}
		rendered, _ := outs["rendered"].Data.(string)
		docs := parseDocs(t, rendered)
		if len(docs) != 1 || dig(t, docs[0], "kind") != "Deployment" {
			t.Fatalf("docs = %v", docs)
		}
		ctr, _ := dig(t, docs[0], "spec", "template", "spec", "containers").([]any)
		c0, _ := ctr[0].(map[string]any)
		if _, present := c0["ports"]; present {
			t.Errorf("ports key must be absent without port: %v", c0)
		}
		if got := dig(t, docs[0], "spec", "replicas"); got != 1 {
			t.Errorf("default replicas = %v, want 1", got)
		}
		if strings.Contains(rendered, "kind: Service") {
			t.Error("no Service without a port")
		}
	})

	t.Run("replicas explicit zero", func(t *testing.T) {
		outs, err := AppManifests{}.Run(ctx, map[string]any{"name": "paused", "image": "img", "replicas": 0}, in)
		if err != nil {
			t.Fatal(err)
		}
		rendered, _ := outs["rendered"].Data.(string)
		if !strings.Contains(rendered, "replicas: 0") {
			t.Errorf("rendered missing replicas: 0:\n%s", rendered)
		}
	})

	t.Run("empty env emits no env key", func(t *testing.T) {
		outs, err := AppManifests{}.Run(ctx, map[string]any{"name": "e", "image": "img", "env": map[string]any{}}, in)
		if err != nil {
			t.Fatal(err)
		}
		rendered, _ := outs["rendered"].Data.(string)
		if strings.Contains(rendered, "env:") {
			t.Errorf("empty env must not emit an env key:\n%s", rendered)
		}
	})

	t.Run("missing target namespace", func(t *testing.T) {
		_, err := AppManifests{}.Run(ctx, map[string]any{"name": "x", "image": "img"}, Inputs{})
		if err == nil || !strings.Contains(err.Error(), "target namespace is required") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		c, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := (AppManifests{}).Run(c, map[string]any{"name": "x", "image": "img"}, in)
		if err == nil {
			t.Fatal("want context error")
		}
	})

	t.Run("config-only fields never leak into the render", func(t *testing.T) {
		outs, err := AppManifests{}.Run(ctx, map[string]any{"name": "n", "image": "img", "out": "gen/n.yaml"}, in)
		if err != nil {
			t.Fatal(err)
		}
		rendered, _ := outs["rendered"].Data.(string)
		if strings.Contains(rendered, "out:") {
			t.Errorf("out is config-only, leaked into render:\n%s", rendered)
		}
	})
}

// TestAppManifests_Deterministic is the renderability contract: same
// config in, byte-identical canonical stream out — regardless of map
// iteration order (env keys are sorted, yaml.Marshal sorts the rest).
func TestAppManifests_Deterministic(t *testing.T) {
	ctx := context.Background()
	in := Inputs{Target: schema.Target{Namespace: "ns"}}
	build := func(envKeys []string) map[string]any {
		env := map[string]any{}
		for _, k := range envKeys {
			env[k] = k + "-value"
		}
		return map[string]any{"name": "det", "image": "img:1", "replicas": 3, "port": 9090, "env": env}
	}

	a, err := AppManifests{}.Run(ctx, build([]string{"B", "A", "C"}), in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := AppManifests{}.Run(ctx, build([]string{"C", "B", "A"}), in)
	if err != nil {
		t.Fatal(err)
	}
	sa, _ := a["rendered"].Data.(string)
	sb, _ := b["rendered"].Data.(string)
	if sa != sb {
		t.Errorf("same config, different bytes:\n--- A ---\n%s--- B ---\n%s", sa, sb)
	}
	c, err := AppManifests{}.Run(ctx, build([]string{"B", "A", "C"}), in)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := c["rendered"].Data.(string)
	if sa != sc {
		t.Error("repeated run is not byte-identical")
	}
}

func TestAppManifests_MaterializedPath(t *testing.T) {
	// A truly absolute path per platform: rooted on POSIX, needs a
	// volume on Windows (filepath.IsAbs("\gen") is false there).
	abs := filepath.Join(string(filepath.Separator), "gen", "x.yaml")
	if runtime.GOOS == "windows" {
		abs = `C:\gen\x.yaml`
	}
	cases := []struct {
		name  string
		cfg   map[string]any
		in    Inputs
		want  string // empty string means "no path, ephemeral"
		flow  bool
		wantE bool
	}{
		{"unset is ephemeral", map[string]any{"name": "x", "image": "img"}, Inputs{FlowFile: "/f/flow.yaml"}, "", false, false},
		{"relative to flow file", map[string]any{"name": "x", "image": "img", "out": "gen/x.yaml"},
			Inputs{FlowFile: filepath.Join("proj", "flow.yaml")}, "", true, false},
		{"absolute passes through", map[string]any{"name": "x", "image": "img", "out": abs},
			Inputs{FlowFile: filepath.Join("proj", "flow.yaml")}, filepath.ToSlash(abs), false, false},
		{"out without flow file", map[string]any{"name": "x", "image": "img", "out": "gen/x.yaml"}, Inputs{}, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AppManifests{}.MaterializedPath(tc.cfg, tc.in)
			if tc.wantE {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, hasOut := tc.cfg["out"]; !hasOut {
				// Ephemeral: no path, no error.
				if got != "" {
					t.Errorf("path = %q, want empty (ephemeral)", got)
				}
				return
			}
			if tc.want != "" {
				if got != tc.want {
					t.Errorf("path = %q, want %q", got, tc.want)
				}
				return
			}
			// Relative case: expect flow-dir join, slash-normalized, no
			// backslashes on any platform (this is the Windows-relevant
			// assertion — filepath.Join on Windows yields "\" and
			// MaterializedPath must normalize them to "/").
			want := filepath.ToSlash(filepath.Join(filepath.Dir(tc.in.FlowFile), filepath.FromSlash(tc.cfg["out"].(string))))
			if got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
			if strings.Contains(got, `\`) {
				t.Errorf("path %q must be slash-normalized", got)
			}
		})
	}
}

func TestAppManifests_CacheInputs(t *testing.T) {
	cfg := map[string]any{"name": "x", "image": "img", "out": "gen/x.yaml"}
	base := Inputs{Target: schema.Target{Namespace: "ns", Context: "ctx"}}
	extra, err := AppManifests{}.CacheInputs(cfg, base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(extra, "ns=ns") || !strings.Contains(extra, "ctx=ctx") {
		t.Errorf("extra = %q", extra)
	}
	// Same target, different flow file: out is config, so identical extras.
	other := Inputs{Target: schema.Target{Namespace: "ns", Context: "ctx"}, FlowFile: "elsewhere/flow.yaml"}
	extra2, err := AppManifests{}.CacheInputs(cfg, other)
	if err != nil {
		t.Fatal(err)
	}
	if extra != extra2 {
		t.Errorf("extras differ across flow files: %q vs %q", extra, extra2)
	}
	// Different namespace or context changes the rendered object → different cache key.
	for _, mutate := range []func(*Inputs){
		func(in *Inputs) { in.Target.Namespace = "other" },
		func(in *Inputs) { in.Target.Context = "other" },
	} {
		in := base
		mutate(&in)
		extra3, err := AppManifests{}.CacheInputs(cfg, in)
		if err != nil {
			t.Fatal(err)
		}
		if extra3 == extra {
			t.Errorf("mutated inputs must change the cache extra: %q", extra3)
		}
	}
}

// TestAppManifests_RegistryWiring guards the production registry: the
// type must be reachable by name for flows, plan, render, and apply.
func TestAppManifests_RegistryWiring(t *testing.T) {
	flow := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: app
      type: app-manifests
      config:
        name: hello
        image: nginx:1.27-alpine
        port: 8080
        out: gen/hello.yaml
`
	cf, err := compile(t, flow)
	if err != nil {
		t.Fatal(err)
	}
	if cf == nil {
		t.Fatal("compile returned nil")
	}
}
