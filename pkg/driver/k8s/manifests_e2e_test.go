package k8s

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/engine"
	"github.com/MRGHOSJ/afon/pkg/schema"
)

// renderManifest materializes content under a temp dir and renders it
// through the real k8s-manifests node — the same path a flow takes.
func renderManifest(t *testing.T, filename, content, namespace string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	in := engine.Inputs{
		FlowFile: filepath.Join(dir, "flow.yaml"),
		Target:   schema.Target{Namespace: namespace},
	}
	outs, err := engine.K8sManifests{}.Run(context.Background(),
		map[string]any{"paths": []any{filename}}, in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered, ok := outs["rendered"].Data.(string)
	if !ok {
		t.Fatalf("rendered output = %T, want string", outs["rendered"].Data)
	}
	return rendered
}

// desiredObjects parses a rendered stream through the real k8s-apply
// node, returning the driver objects it produces.
func desiredObjects(t *testing.T, rendered, namespace string) []driver.Object {
	t.Helper()
	in := engine.Inputs{
		FlowFile: "flow.yaml",
		Target:   schema.Target{Namespace: namespace},
	}
	objs, err := engine.K8sApply{}.Desired(context.Background(),
		map[string]any{"manifests": rendered}, in)
	if err != nil {
		t.Fatalf("Desired: %v", err)
	}
	return objs
}

// TestPlan_NamespaceMismatchFromManifests pins fill-if-absent: a
// manifest declaring its own namespace is not rewritten by rendering,
// and the plan rejects it through the driver's mismatch check.
func TestPlan_NamespaceMismatchFromManifests(t *testing.T) {
	rendered := renderManifest(t, "cm.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: other-ns
data:
  k: v
`, testNS)
	if !strings.Contains(rendered, "namespace: other-ns") {
		t.Fatalf("declared namespace was rewritten:\n%s", rendered)
	}
	if strings.Contains(rendered, "namespace: "+testNS) {
		t.Fatalf("target namespace was injected over a declared one:\n%s", rendered)
	}

	objs := desiredObjects(t, rendered, testNS)
	if len(objs) != 1 || objs[0].Namespace != "other-ns" {
		t.Fatalf("objects = %+v, want the declared namespace", objs)
	}

	d, _ := newTestDriver(t, false, nil, seedNamespace())
	_, err := d.Plan(context.Background(), testOwner, objs)
	if err == nil || !strings.Contains(err.Error(), "manages") {
		t.Fatalf("err = %v, want namespace mismatch rejection at plan time", err)
	}
}

// TestPlan_ClusterScopedFromManifests runs a ClusterRole from a real
// manifest render through k8s-apply and the fake cluster: the plan
// must surface the typed ErrClusterScoped.
func TestPlan_ClusterScopedFromManifests(t *testing.T) {
	rendered := renderManifest(t, "role.yaml", `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: reader
rules: []
`, testNS)

	objs := desiredObjects(t, rendered, testNS)
	if len(objs) != 1 {
		t.Fatalf("objects = %d, want 1", len(objs))
	}
	if objs[0].Group != "rbac.authorization.k8s.io" || objs[0].Kind != "ClusterRole" {
		t.Fatalf("object identity = %+v", objs[0])
	}

	d, _ := newTestDriver(t, false, nil, seedNamespace())
	_, err := d.Plan(context.Background(), testOwner, objs)
	var cs *driver.ErrClusterScoped
	if !errors.As(err, &cs) {
		t.Fatalf("err = %v, want ErrClusterScoped", err)
	}
	if cs.Kind != "ClusterRole" || cs.Group != "rbac.authorization.k8s.io" {
		t.Errorf("ErrClusterScoped = %+v", cs)
	}
}
