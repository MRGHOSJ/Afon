package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/schema"
)

// --- helpers -------------------------------------------------------------

// writeManifestFiles materializes files under a fresh temp dir and
// returns the flow-file path (the anchor globs resolve against — the
// file itself need not exist).
func writeManifestFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "flow.yaml")
}

func manifestInputs(flowFile, namespace string) Inputs {
	return Inputs{
		FlowFile: flowFile,
		Target:   schema.Target{Namespace: namespace, Context: "test-ctx"},
	}
}

func runManifests(t *testing.T, cfg map[string]any, in Inputs) string {
	t.Helper()
	outs, err := K8sManifests{}.Run(context.Background(), cfg, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rendered, ok := outs["rendered"].Data.(string)
	if !ok {
		t.Fatalf("rendered output = %T, want string", outs["rendered"].Data)
	}
	return rendered
}

const cmDoc = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
data:
  mark: %s
`

// --- Validate ------------------------------------------------------------

func TestK8sManifests_Validate(t *testing.T) {
	cases := []struct {
		name string
		cfg  any
		want string // error substring; "" means valid
	}{
		{"not a mapping", []any{}, "must be a mapping"},
		{"missing paths", map[string]any{}, `missing required field "paths"`},
		{"paths not a list", map[string]any{"paths": "x.yaml"}, "non-empty list"},
		{"paths empty", map[string]any{"paths": []any{}}, "non-empty list"},
		{"paths element empty", map[string]any{"paths": []any{""}}, "paths[0] must not be empty"},
		{"paths element int", map[string]any{"paths": []any{7}}, "paths[0] must be a string"},
		{"images not a mapping", map[string]any{"paths": []any{"a.yaml"}, "images": "x"}, "images must be a mapping"},
		{"images value empty", map[string]any{"paths": []any{"a.yaml"}, "images": map[string]any{"app": ""}}, `images["app"] must not be empty`},
		{"unknown field", map[string]any{"paths": []any{"a.yaml"}, "overlay": true}, `unknown field "overlay"`},
		{"valid", map[string]any{"paths": []any{"deploy/*.yaml"}}, ""},
		{"valid with images", map[string]any{
			"paths":  []any{"a.yaml"},
			"images": map[string]any{"app": "nginx@sha256:00"},
		}, ""},
		{"reference paths", map[string]any{"paths": []any{Reference{Node: "x", Output: "paths"}}}, ""},
		{"reference image", map[string]any{
			"paths":  []any{"a.yaml"},
			"images": map[string]any{"app": Reference{Node: "img", Output: "digest"}},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := K8sManifests{}.Validate(tc.cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// --- Run -----------------------------------------------------------------

func TestK8sManifests_Run(t *testing.T) {
	t.Run("sorted file order with separators", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{
			"b.yaml": fmt.Sprintf(cmDoc, "bravo"),
			"a.yaml": fmt.Sprintf(cmDoc, "alpha"),
		})
		rendered := runManifests(t,
			map[string]any{"paths": []any{"*.yaml"}},
			manifestInputs(flow, "staging"))
		ia, ib := strings.Index(rendered, "alpha"), strings.Index(rendered, "bravo")
		if ia < 0 || ib < 0 || ia > ib {
			t.Fatalf("file order wrong (alpha@%d bravo@%d):\n%s", ia, ib, rendered)
		}
		if !strings.Contains(rendered, "---\n") {
			t.Fatalf("missing document separator:\n%s", rendered)
		}
	})

	t.Run("namespace injected when absent", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{"a.yaml": fmt.Sprintf(cmDoc, "x")})
		rendered := runManifests(t,
			map[string]any{"paths": []any{"a.yaml"}},
			manifestInputs(flow, "staging"))
		if !strings.Contains(rendered, "namespace: staging") {
			t.Fatalf("target namespace not injected:\n%s", rendered)
		}
	})

	t.Run("metadata created when missing", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{"a.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
`})
		rendered := runManifests(t,
			map[string]any{"paths": []any{"a.yaml"}},
			manifestInputs(flow, "staging"))
		if !strings.Contains(rendered, "namespace: staging") {
			t.Fatalf("namespace not injected into missing metadata:\n%s", rendered)
		}
	})

	t.Run("declared namespace is not rewritten", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{"a.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: other-ns
`})
		rendered := runManifests(t,
			map[string]any{"paths": []any{"a.yaml"}},
			manifestInputs(flow, "staging"))
		if !strings.Contains(rendered, "namespace: other-ns") {
			t.Fatalf("declared namespace was rewritten:\n%s", rendered)
		}
		if strings.Contains(rendered, "namespace: staging") {
			t.Fatalf("target namespace was injected over a declared one:\n%s", rendered)
		}
	})

	t.Run("image override across pod shapes", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{
			"deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      initContainers:
        - name: init
          image: init:1
      containers:
        - name: app
          image: old-app:1
        - name: sidecar
          image: side:1
`,
			"cron.yaml": `apiVersion: batch/v1
kind: CronJob
metadata:
  name: nightly
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: jobapp
              image: old-job:1
`,
			"pod.yaml": `apiVersion: v1
kind: Pod
metadata:
  name: bare
spec:
  containers:
    - name: podapp
      image: old-pod:1
`,
		})
		rendered := runManifests(t,
			map[string]any{
				"paths": []any{"*.yaml"},
				"images": map[string]any{
					"app":    "new-app@sha256:aa",
					"jobapp": "new-job@sha256:bb",
					"podapp": "new-pod@sha256:cc",
				},
			},
			manifestInputs(flow, "staging"))
		for _, want := range []string{
			"new-app@sha256:aa", "new-job@sha256:bb", "new-pod@sha256:cc",
			"image: side:1", "image: init:1", // untouched
		} {
			if !strings.Contains(rendered, want) {
				t.Errorf("rendered missing %q:\n%s", want, rendered)
			}
		}
		if strings.Contains(rendered, "old-app:1") || strings.Contains(rendered, "old-job:1") || strings.Contains(rendered, "old-pod:1") {
			t.Errorf("an overridden image survived:\n%s", rendered)
		}
	})

	t.Run("image override no-match lists containers", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{"a.yaml": `apiVersion: v1
kind: Pod
metadata:
  name: p
spec:
  containers:
    - name: zulu
      image: z:1
    - name: alpha
      image: a:1
`})
		_, err := K8sManifests{}.Run(context.Background(),
			map[string]any{"paths": []any{"a.yaml"}, "images": map[string]any{"nope": "x:1"}},
			manifestInputs(flow, "staging"))
		if err == nil {
			t.Fatal("no-match override must fail")
		}
		for _, want := range []string{`no container named "nope"`, "alpha, zulu"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to contain %q", err, want)
			}
		}
	})

	t.Run("no match names the pattern", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{"a.yaml": fmt.Sprintf(cmDoc, "x")})
		_, err := K8sManifests{}.Run(context.Background(),
			map[string]any{"paths": []any{"missing/*.yaml"}},
			manifestInputs(flow, "staging"))
		if err == nil || !strings.Contains(err.Error(), `no files match "missing/*.yaml"`) {
			t.Fatalf("err = %v, want the offending pattern", err)
		}
	})

	t.Run("bad yaml names the file", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{"broken.yaml": "key: [unclosed\n"})
		_, err := K8sManifests{}.Run(context.Background(),
			map[string]any{"paths": []any{"broken.yaml"}},
			manifestInputs(flow, "staging"))
		if err == nil || !strings.Contains(err.Error(), "broken.yaml") {
			t.Fatalf("err = %v, want the file name", err)
		}
	})

	t.Run("zero documents lists the files", func(t *testing.T) {
		flow := writeManifestFiles(t, map[string]string{
			"a.yaml": "# only a comment\n",
			"b.yaml": "---\n---\n",
		})
		_, err := K8sManifests{}.Run(context.Background(),
			map[string]any{"paths": []any{"*.yaml"}},
			manifestInputs(flow, "staging"))
		if err == nil {
			t.Fatal("a comment-only render must fail")
		}
		for _, want := range []string{"no documents found", "a.yaml", "b.yaml"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to contain %q", err, want)
			}
		}
	})

	t.Run("requires a flow file", func(t *testing.T) {
		_, err := K8sManifests{}.Run(context.Background(),
			map[string]any{"paths": []any{"a.yaml"}},
			Inputs{Target: schema.Target{Namespace: "staging"}})
		if err == nil || !strings.Contains(err.Error(), "flow file") {
			t.Fatalf("err = %v, want flow-file error", err)
		}
	})
}

// TestK8sManifests_CanonicalShuffledKeys is the real rendering
// guarantee: the same logical documents with keys in different orders
// render byte-identically, so the output (and therefore the downstream
// cache key) depends only on content.
func TestK8sManifests_CanonicalShuffledKeys(t *testing.T) {
	docA := `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: staging
data:
  zeta: "1"
  alpha: "2"
  mid: "3"
`
	docB := `data:
  mid: "3"
  alpha: "2"
  zeta: "1"
metadata:
  namespace: staging
  name: cfg
kind: ConfigMap
apiVersion: v1
`
	flowA := writeManifestFiles(t, map[string]string{"a.yaml": docA})
	renderA := runManifests(t,
		map[string]any{"paths": []any{"a.yaml"}},
		manifestInputs(flowA, "staging"))
	flowB := writeManifestFiles(t, map[string]string{"a.yaml": docB})
	renderB := runManifests(t,
		map[string]any{"paths": []any{"a.yaml"}},
		manifestInputs(flowB, "staging"))
	if renderA != renderB {
		t.Fatalf("shuffled keys changed the rendering:\n--- A ---\n%s--- B ---\n%s", renderA, renderB)
	}
}

// --- CacheInputs ---------------------------------------------------------

func TestK8sManifests_CacheInputs(t *testing.T) {
	cfg := map[string]any{"paths": []any{"deploy/*.yaml"}}
	files := map[string]string{
		"deploy/a.yaml": fmt.Sprintf(cmDoc, "alpha"),
		"deploy/b.yaml": fmt.Sprintf(cmDoc, "bravo"),
	}
	flow := writeManifestFiles(t, files)
	dir := filepath.Dir(flow)
	cacheKey := func(in Inputs) string {
		t.Helper()
		extra, err := K8sManifests{}.CacheInputs(cfg, in)
		if err != nil {
			t.Fatalf("CacheInputs: %v", err)
		}
		return extra
	}
	staging := manifestInputs(flow, "staging")

	base := cacheKey(staging)

	t.Run("deterministic", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			if got := cacheKey(staging); got != base {
				t.Fatalf("key drifted on call %d:\n%s\nvs\n%s", i, got, base)
			}
		}
	})

	// The files live in the same directory across mutations; each of
	// edit / delete / add must change the key (the three file-level
	// non-negotiables), and so must the target.
	rewrite := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(name string) {
		t.Helper()
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("edit changes the key", func(t *testing.T) {
		rewrite("deploy/a.yaml", fmt.Sprintf(cmDoc, "edited"))
		defer rewrite("deploy/a.yaml", fmt.Sprintf(cmDoc, "alpha"))
		if got := cacheKey(staging); got == base {
			t.Fatal("editing a manifest did not change the key")
		}
	})

	t.Run("delete changes the key", func(t *testing.T) {
		remove("deploy/b.yaml")
		defer rewrite("deploy/b.yaml", fmt.Sprintf(cmDoc, "bravo"))
		if got := cacheKey(staging); got == base {
			t.Fatal("deleting a manifest did not change the key")
		}
	})

	t.Run("add changes the key", func(t *testing.T) {
		rewrite("deploy/c.yaml", fmt.Sprintf(cmDoc, "charlie"))
		defer remove("deploy/c.yaml")
		if got := cacheKey(staging); got == base {
			t.Fatal("adding a manifest did not change the key")
		}
	})

	t.Run("target changes the key", func(t *testing.T) {
		if got := cacheKey(manifestInputs(flow, "production")); got == base {
			t.Fatal("target namespace did not change the key")
		}
	})

	t.Run("missing file names the pattern", func(t *testing.T) {
		remove("deploy/a.yaml")
		defer rewrite("deploy/a.yaml", fmt.Sprintf(cmDoc, "alpha"))
		strict := map[string]any{"paths": []any{"deploy/a.yaml"}}
		_, err := K8sManifests{}.CacheInputs(strict, staging)
		if err == nil || !strings.Contains(err.Error(), `no files match "deploy/a.yaml"`) {
			t.Fatalf("err = %v, want the offending pattern", err)
		}
	})
}

// --- plan-level cache invalidation ---------------------------------------

const k8sManifestFlowTpl = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  target: {namespace: %s}
  nodes:
    - id: m
      type: k8s-manifests
      config:
        paths: [deploy/*.yaml]
`

// TestK8sManifests_PlanCacheInvalidation runs the file-level
// non-negotiables through BuildPlan/Apply end to end: each mutation
// must take the plan from cached to will-run, and re-apply must make
// it cached again.
func TestK8sManifests_PlanCacheInvalidation(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(K8sManifests{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store := NewMemoryStore()

	flow := writeManifestFiles(t, map[string]string{
		"deploy/a.yaml": fmt.Sprintf(cmDoc, "alpha"),
		"deploy/b.yaml": fmt.Sprintf(cmDoc, "bravo"),
	})
	compileFlow := func(ns string) *CompiledFlow {
		t.Helper()
		cf, err := compileWith(t, fmt.Sprintf(k8sManifestFlowTpl, ns), reg)
		if err != nil {
			t.Fatal(err)
		}
		// Point the compiled flow at the temp dir: Compile keeps the
		// source name, and globs anchor on cf.File.
		cf.File = flow
		return cf
	}
	planEntry := func(cf *CompiledFlow) (status, key string) {
		t.Helper()
		p, err := BuildPlan(ctx, cf, store, nilDriver())
		if err != nil {
			t.Fatal(err)
		}
		return p.Entries[0].Status, p.Entries[0].Key
	}
	apply := func(cf *CompiledFlow) {
		t.Helper()
		if _, err := Apply(ctx, cf, store, nilDriver(), ExecutorOptions{}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	cf := compileFlow("staging")
	if status, key := planEntry(cf); status != PlanWillRun || key == "" {
		t.Fatalf("cold plan = %q/%q, want will-run with a key", status, key)
	}
	apply(cf)
	status, key1 := planEntry(cf)
	if status != PlanCached {
		t.Fatalf("warm plan = %q, want cached", status)
	}

	t.Run("edit misses", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(filepath.Dir(flow), "deploy", "a.yaml"),
			[]byte(fmt.Sprintf(cmDoc, "edited")), 0o644); err != nil {
			t.Fatal(err)
		}
		status, key := planEntry(cf)
		if status == PlanCached {
			t.Fatal("edit did not invalidate the cache")
		}
		if key == key1 {
			t.Fatal("edit did not change the key")
		}
		apply(cf)
		if status, _ := planEntry(cf); status != PlanCached {
			t.Fatalf("re-apply plan = %q, want cached", status)
		}
	})

	t.Run("add misses", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(filepath.Dir(flow), "deploy", "c.yaml"),
			[]byte(fmt.Sprintf(cmDoc, "charlie")), 0o644); err != nil {
			t.Fatal(err)
		}
		if status, _ := planEntry(cf); status == PlanCached {
			t.Fatal("adding a file did not invalidate the cache")
		}
		apply(cf)
		if status, _ := planEntry(cf); status != PlanCached {
			t.Fatalf("re-apply plan = %q, want cached", status)
		}
	})

	t.Run("delete misses", func(t *testing.T) {
		// Delete a file whose resulting state was never cached: the
		// engine may legitimately serve an older identical state, so
		// the test must make the post-delete state genuinely new.
		if err := os.Remove(filepath.Join(filepath.Dir(flow), "deploy", "b.yaml")); err != nil {
			t.Fatal(err)
		}
		status, key := planEntry(cf)
		if status == PlanCached {
			t.Fatal("deleting a file did not invalidate the cache")
		}
		if key == key1 {
			t.Fatal("delete did not change the key")
		}
		apply(cf)
		if status, _ := planEntry(cf); status != PlanCached {
			t.Fatalf("re-apply plan = %q, want cached", status)
		}
	})

	t.Run("target change misses", func(t *testing.T) {
		prod := compileFlow("production")
		status, key := planEntry(prod)
		if status == PlanCached {
			t.Fatal("target change did not invalidate the cache")
		}
		if key == key1 {
			t.Fatal("target change did not change the key")
		}
	})
}

// --- k8s-apply -----------------------------------------------------------

func TestK8sApply_Validate(t *testing.T) {
	cases := []struct {
		name string
		cfg  any
		want string
	}{
		{"not a mapping", []any{}, "must be a mapping"},
		{"missing manifests", map[string]any{}, `missing required field "manifests"`},
		{"empty literal", map[string]any{"manifests": ""}, "must not be empty"},
		{"not a string", map[string]any{"manifests": 42}, "must be a string"},
		{"unknown field", map[string]any{"manifests": "x", "prune": true}, `unknown field "prune"`},
		{"valid", map[string]any{"manifests": "apiVersion: v1\n"}, ""},
		{"reference", map[string]any{"manifests": Reference{Node: "m", Output: "rendered"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := K8sApply{}.Validate(tc.cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestK8sApply_Desired(t *testing.T) {
	in := manifestInputs("flow.yaml", "staging")
	desired := func(t *testing.T, manifests string) []driver.Object {
		t.Helper()
		objs, err := K8sApply{}.Desired(context.Background(),
			map[string]any{"manifests": manifests}, in)
		if err != nil {
			t.Fatalf("Desired: %v", err)
		}
		return objs
	}

	t.Run("multi-doc identity and data", func(t *testing.T) {
		objs := desired(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: staging
data:
  k: v
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 2
`)
		if len(objs) != 2 {
			t.Fatalf("got %d objects, want 2", len(objs))
		}
		cm := objs[0]
		if cm.Group != "" || cm.Version != "v1" || cm.Kind != "ConfigMap" ||
			cm.Name != "cfg" || cm.Namespace != "staging" {
			t.Errorf("configmap identity = %+v", cm)
		}
		data, ok := cm.Data["data"].(map[string]any)
		if !ok || data["k"] != "v" {
			t.Errorf("Data = %v, want the whole document", cm.Data)
		}
		dep := objs[1]
		if dep.Group != "apps" || dep.Version != "v1" || dep.Kind != "Deployment" ||
			dep.Name != "web" || dep.Namespace != "" {
			t.Errorf("deployment identity = %+v", dep)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		_, err := K8sApply{}.Desired(context.Background(),
			map[string]any{"manifests": "  \n"}, in)
		if err == nil || !strings.Contains(err.Error(), "input is empty") {
			t.Fatalf("err = %v, want empty-input error", err)
		}
	})

	t.Run("comments only", func(t *testing.T) {
		_, err := K8sApply{}.Desired(context.Background(),
			map[string]any{"manifests": "# nothing here\n"}, in)
		if err == nil || !strings.Contains(err.Error(), "no mapping documents") {
			t.Fatalf("err = %v, want no-objects error", err)
		}
	})

	t.Run("scalar document", func(t *testing.T) {
		_, err := K8sApply{}.Desired(context.Background(),
			map[string]any{"manifests": "just a string\n"}, in)
		if err == nil || !strings.Contains(err.Error(), "document #1 is not a mapping") {
			t.Fatalf("err = %v, want document index", err)
		}
	})

	t.Run("missing kind", func(t *testing.T) {
		_, err := K8sApply{}.Desired(context.Background(),
			map[string]any{"manifests": "apiVersion: v1\nmetadata:\n  name: x\n"}, in)
		if err == nil || !strings.Contains(err.Error(), "document #1: kind is required") {
			t.Fatalf("err = %v, want kind error with index", err)
		}
	})

	t.Run("missing name", func(t *testing.T) {
		_, err := K8sApply{}.Desired(context.Background(),
			map[string]any{"manifests": "apiVersion: v1\nkind: ConfigMap\n"}, in)
		if err == nil || !strings.Contains(err.Error(), "metadata.name is required") {
			t.Fatalf("err = %v, want name error", err)
		}
	})

	t.Run("metadata not a mapping", func(t *testing.T) {
		_, err := K8sApply{}.Desired(context.Background(),
			map[string]any{"manifests": "apiVersion: v1\nkind: ConfigMap\nmetadata: nope\n"}, in)
		if err == nil || !strings.Contains(err.Error(), "metadata must be a mapping") {
			t.Fatalf("err = %v, want metadata type error", err)
		}
	})

	t.Run("unresolved manifests", func(t *testing.T) {
		_, err := K8sApply{}.Desired(context.Background(),
			map[string]any{"manifests": Reference{Node: "m", Output: "rendered"}}, in)
		if err == nil || !strings.Contains(err.Error(), "resolved string") {
			t.Fatalf("err = %v, want resolved-string error", err)
		}
	})
}
