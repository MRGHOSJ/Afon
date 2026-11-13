package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// GVRs shared with the spike (same package, one definition).
var (
	svcGVR    = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	cmGVR     = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	deployGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	stsGVR    = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	dsGVR     = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	jobGVR    = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	podGVR    = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

const (
	testNS   = "test-ns"
	testFlow = "web"
	testNode = "apply"
)

var testOwner = driver.Owner{Flow: testFlow, Node: testNode}

// staticMapper resolves exactly the kinds the tests register; anything
// else gets the same NoKindMatchError discovery would produce for an
// unknown type.
type staticMapper map[schema.GroupVersionKind]*meta.RESTMapping

func (m staticMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	try := func(v string) (*meta.RESTMapping, bool) {
		mapping, ok := m[schema.GroupVersionKind{Group: gk.Group, Version: v, Kind: gk.Kind}]
		return mapping, ok
	}
	if len(versions) > 0 {
		for _, v := range versions {
			if mapping, ok := try(v); ok {
				return mapping, nil
			}
		}
	} else {
		// No version requested: prefer the lowest registered, stable.
		best := ""
		for gvk := range m {
			if gvk.Group != gk.Group || gvk.Kind != gk.Kind {
				continue
			}
			if best == "" || gvk.Version < best {
				best = gvk.Version
			}
		}
		if best != "" {
			if mapping, ok := try(best); ok {
				return mapping, nil
			}
		}
	}
	return nil, &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
}

func namespaced(group, version, kind, resource string) *meta.RESTMapping {
	return &meta.RESTMapping{
		GroupVersionKind: schema.GroupVersionKind{Group: group, Version: version, Kind: kind},
		Resource:         schema.GroupVersionResource{Group: group, Version: version, Resource: resource},
		Scope:            meta.RESTScopeNamespace,
	}
}

func testMapper() staticMapper {
	return staticMapper{
		{Version: "v1", Kind: "ConfigMap"}:                  namespaced("", "v1", "ConfigMap", "configmaps"),
		{Version: "v1", Kind: "Service"}:                    namespaced("", "v1", "Service", "services"),
		{Version: "v1", Kind: "Pod"}:                        namespaced("", "v1", "Pod", "pods"),
		{Group: "apps", Version: "v1", Kind: "Deployment"}:  namespaced("apps", "v1", "Deployment", "deployments"),
		{Group: "apps", Version: "v1", Kind: "StatefulSet"}: namespaced("apps", "v1", "StatefulSet", "statefulsets"),
		{Group: "apps", Version: "v1", Kind: "DaemonSet"}:   namespaced("apps", "v1", "DaemonSet", "daemonsets"),
		{Group: "batch", Version: "v1", Kind: "Job"}:        namespaced("batch", "v1", "Job", "jobs"),
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}: {
			GroupVersionKind: schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
			Resource:         schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
			Scope:            meta.RESTScopeRoot,
		},
	}
}

// installSSAFake adds the behaviors the plain fake tracker lacks:
// dry-run applies that compute a merged result without persisting,
// SSA conflicts (the tracker never emits 409s), create-via-apply (the
// plain tracker's Apply returns NotFound on an absent object, where a
// real server creates it), and update-via-apply (the plain tracker
// strategic-merges, which fail-fasts on unstructured). Every
// ApplyPatchType patch therefore runs through this reactor.
func installSSAFake(dyn *fake.FakeDynamicClient, conflicts map[string]string) {
	dyn.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(k8stesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		if mgr, hit := conflicts[pa.GetName()]; hit {
			if pa.PatchOptions.Force == nil || !*pa.PatchOptions.Force {
				// The exact message shape the spike captured from a
				// live server — this is what asConflict parses.
				return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{
					Status:  "Failure",
					Reason:  metav1.StatusReasonConflict,
					Message: fmt.Sprintf(`Apply failed with 1 conflict: conflict with %q: .metadata.labels.team`, mgr),
				}}
			}
		}
		body := map[string]any{}
		if err := json.Unmarshal(pa.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		patchObj := &unstructured.Unstructured{Object: body}
		patchObj.SetName(pa.GetName())
		patchObj.SetNamespace(pa.GetNamespace())

		tr := dyn.Tracker()
		result := patchObj
		exists := false
		live, err := tr.Get(pa.GetResource(), pa.GetNamespace(), pa.GetName(), metav1.GetOptions{})
		if err == nil {
			exists = true
			liveMap, err := toMap(live)
			if err != nil {
				return true, nil, err
			}
			result = &unstructured.Unstructured{Object: mergeMaps(liveMap, body)}
		} else if !apierrors.IsNotFound(err) {
			return true, nil, err
		}

		if len(pa.PatchOptions.DryRun) > 0 {
			return true, result, nil // dry-run: compute, never persist
		}
		if exists {
			err = tr.Update(pa.GetResource(), result, pa.GetNamespace(), metav1.UpdateOptions{
				FieldManager: pa.PatchOptions.FieldManager,
			})
		} else {
			err = tr.Create(pa.GetResource(), result, pa.GetNamespace(), metav1.CreateOptions{
				FieldManager: pa.PatchOptions.FieldManager,
			})
		}
		if err != nil {
			return true, nil, err
		}
		return true, result, nil
	})
}

// toMap deep-copies a live object through JSON so the merge never
// aliases tracker state.
func toMap(obj runtime.Object) (map[string]any, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mergeMaps is RFC 7386 JSON Merge Patch: objects merge recursively,
// null deletes, everything else (arrays included) replaces. That is
// the shape SSA has for maps; list merge-key semantics belong to the
// live spike, not this harness — no fixture here depends on them.
func mergeMaps(old, patch map[string]any) map[string]any {
	out := make(map[string]any, len(old)+len(patch))
	for k, v := range old {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		pm, isPatchMap := v.(map[string]any)
		om, isOldMap := out[k].(map[string]any)
		if isPatchMap && isOldMap {
			out[k] = mergeMaps(om, pm)
			continue
		}
		out[k] = v
	}
	return out
}

// newTestDriver wires a Driver over a fake cluster seeded with objects,
// pruning on (the default every existing test expects).
func newTestDriver(t *testing.T, force bool, conflicts map[string]string, seeds ...runtime.Object) (*Driver, *fake.FakeDynamicClient) {
	t.Helper()
	return newTestDriverOpts(t, force, true, conflicts, seeds...)
}

// newTestDriverOpts is newTestDriver with explicit prune control.
func newTestDriverOpts(t *testing.T, force, prune bool, conflicts map[string]string, seeds ...runtime.Object) (*Driver, *fake.FakeDynamicClient) {
	t.Helper()
	listKinds := map[schema.GroupVersionResource]string{
		cmGVR: "ConfigMapList", svcGVR: "ServiceList", deployGVR: "DeploymentList",
		stsGVR: "StatefulSetList", dsGVR: "DaemonSetList", jobGVR: "JobList", podGVR: "PodList",
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, seeds...)
	installSSAFake(dyn, conflicts)
	d, err := New(dyn, testMapper(), testNS, force, prune)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, dyn
}

// --- fixtures ------------------------------------------------------------

func seedNamespace() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": testNS},
	}}
}

// appliedCM is a ConfigMap as a previous afon apply would have left
// it: owner labels stamped, a resourceVersion the strip list must
// ignore.
func appliedCM(name string, owner driver.Owner, data map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": testNS,
			"labels":          map[string]any{flowLabel: owner.Flow, nodeLabel: owner.Node},
			"resourceVersion": "42",
		},
	}
	if data != nil {
		obj["data"] = data
	}
	return &unstructured.Unstructured{Object: obj}
}

// desiredCM is the driver.Object form of the same ConfigMap: Data is
// the full manifest, identity in the Object fields.
func desiredCM(name string, data map[string]any) driver.Object {
	return driver.Object{
		Version: "v1", Kind: "ConfigMap", Namespace: testNS, Name: name,
		Data: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": name, "namespace": testNS},
			"data":     data,
		},
	}
}

func desiredSvc(name string) driver.Object {
	return driver.Object{
		Version: "v1", Kind: "Service", Namespace: testNS, Name: name,
		Data: map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": name, "namespace": testNS},
			"spec":     map[string]any{"type": "ClusterIP"},
		},
	}
}

func desiredDeploy(name string, image string) driver.Object {
	return driver.Object{
		Group: "apps", Version: "v1", Kind: "Deployment", Namespace: testNS, Name: name,
		Data: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": name, "namespace": testNS},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "app", "image": image}},
			}}},
		},
	}
}

// --- Plan ---------------------------------------------------------------

func TestPlan_Classification(t *testing.T) {
	ctx := context.Background()

	t.Run("absent is create and plan stays read-only", func(t *testing.T) {
		d, dyn := newTestDriver(t, false, nil, seedNamespace())
		diffs, err := d.Plan(ctx, testOwner, []driver.Object{desiredCM("cfg", map[string]any{"k": "v"})})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(diffs) != 1 || diffs[0].Op != driver.OpCreate {
			t.Fatalf("diffs = %+v, want one create", diffs)
		}
		if _, err := dyn.Tracker().Get(cmGVR, testNS, "cfg", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("plan persisted the object (err = %v): Plan must be read-only", err)
		}
	})

	t.Run("identical live is none", func(t *testing.T) {
		d, _ := newTestDriver(t, false, nil, seedNamespace(),
			appliedCM("cfg", testOwner, map[string]any{"k": "v"}))
		diffs, err := d.Plan(ctx, testOwner, []driver.Object{desiredCM("cfg", map[string]any{"k": "v"})})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(diffs) != 1 || diffs[0].Op != driver.OpNone {
			t.Fatalf("diffs = %+v, want one none", diffs)
		}
	})

	t.Run("changed live is update", func(t *testing.T) {
		d, _ := newTestDriver(t, false, nil, seedNamespace(),
			appliedCM("cfg", testOwner, map[string]any{"k": "old"}))
		diffs, err := d.Plan(ctx, testOwner, []driver.Object{desiredCM("cfg", map[string]any{"k": "new"})})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(diffs) != 1 || diffs[0].Op != driver.OpUpdate {
			t.Fatalf("diffs = %+v, want one update", diffs)
		}
	})
}

func TestPlan_ForeignOwner(t *testing.T) {
	ctx := context.Background()
	other := driver.Owner{Flow: "payments", Node: "db"}
	d, _ := newTestDriver(t, false, nil, seedNamespace(),
		appliedCM("cfg", other, map[string]any{"k": "v"}))

	_, err := d.Plan(ctx, testOwner, []driver.Object{desiredCM("cfg", map[string]any{"k": "v"})})
	var fe *driver.ErrForeignOwner
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want ErrForeignOwner", err)
	}
	if fe.Flow != "payments" || fe.Node != "db" || fe.Object.Name != "cfg" {
		t.Errorf("ErrForeignOwner = %+v, want payments/db/cfg", fe)
	}
}

func TestPlan_ClusterScoped(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDriver(t, false, nil, seedNamespace())
	obj := driver.Object{
		Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole",
		Name: "role",
	}
	_, err := d.Plan(ctx, testOwner, []driver.Object{obj})
	var cs *driver.ErrClusterScoped
	if !errors.As(err, &cs) {
		t.Fatalf("err = %v, want ErrClusterScoped", err)
	}
	if cs.Kind != "ClusterRole" || cs.Group != "rbac.authorization.k8s.io" {
		t.Errorf("ErrClusterScoped = %+v", cs)
	}
}

func TestPlan_NamespaceMissing(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDriver(t, false, nil) // no Namespace seed
	_, err := d.Plan(ctx, testOwner, []driver.Object{desiredCM("cfg", nil)})
	if err == nil || !contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want missing-namespace message", err)
	}
}

func TestPlan_UnknownKind(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDriver(t, false, nil, seedNamespace())
	obj := driver.Object{Version: "v1", Kind: "Wibble", Namespace: testNS, Name: "x"}
	if _, err := d.Plan(ctx, testOwner, []driver.Object{obj}); err == nil || !contains(err.Error(), "resolving") {
		t.Fatalf("err = %v, want resolution error", err)
	}
}

func TestPlan_Prune(t *testing.T) {
	ctx := context.Background()
	other := driver.Owner{Flow: "payments", Node: "db"}
	d, _ := newTestDriver(t, false, nil, seedNamespace(),
		appliedCM("keep", testOwner, map[string]any{"k": "v"}),
		appliedCM("stale", testOwner, map[string]any{"k": "w"}),
		appliedCM("foreign", other, map[string]any{"k": "x"}),
		// Owned Service: kind present in desired via desiredSvc below,
		// so its stale sibling must be pruned too.
		appliedSvc("keep-svc", testOwner),
		appliedSvc("stale-svc", testOwner),
	)

	diffs, err := d.Plan(ctx, testOwner, []driver.Object{
		desiredCM("keep", map[string]any{"k": "v"}),
		desiredSvc("keep-svc"),
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := map[string]driver.Op{}
	for _, df := range diffs {
		got[df.Object.Kind+"/"+df.Object.Name] = df.Op
	}
	want := map[string]driver.Op{
		"ConfigMap/keep":    driver.OpNone,
		"Service/keep-svc":  driver.OpNone,
		"ConfigMap/stale":   driver.OpDelete,
		"Service/stale-svc": driver.OpDelete,
	}
	if len(got) != len(want) {
		t.Fatalf("diffs = %v, want %v", got, want)
	}
	for k, op := range want {
		if got[k] != op {
			t.Errorf("diffs[%q] = %q, want %q (all: %v)", k, got[k], op, got)
		}
	}
}

func TestPlan_Conflict(t *testing.T) {
	ctx := context.Background()

	t.Run("existing object", func(t *testing.T) {
		d, _ := newTestDriver(t, false, map[string]string{"cfg": "kubectl"},
			seedNamespace(), appliedCM("cfg", testOwner, map[string]any{"k": "v"}))
		_, err := d.Plan(ctx, testOwner, []driver.Object{desiredCM("cfg", map[string]any{"k": "v"})})
		var ce *driver.ErrConflict
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if len(ce.Managers) != 1 || ce.Managers[0] != "kubectl" {
			t.Errorf("Managers = %v, want [kubectl]", ce.Managers)
		}
	})

	t.Run("create is dry-run validated", func(t *testing.T) {
		d, dyn := newTestDriver(t, false, map[string]string{"cfg": "kubectl"}, seedNamespace())
		_, err := d.Plan(ctx, testOwner, []driver.Object{desiredCM("cfg", nil)})
		var ce *driver.ErrConflict
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want ErrConflict from the create dry-run", err)
		}
		if _, err := dyn.Tracker().Get(cmGVR, testNS, "cfg", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("failed plan persisted the object: %v", err)
		}
	})

	// The force flag changes plan, not just apply: the dry-run
	// classification rides the same force setting, so with the flag a
	// conflicting object plans as an update instead of failing.
	t.Run("force plans through the conflict", func(t *testing.T) {
		d, _ := newTestDriver(t, true, map[string]string{"cfg": "kubectl"},
			seedNamespace(), appliedCM("cfg", testOwner, map[string]any{"k": "v"}))
		diffs, err := d.Plan(ctx, testOwner, []driver.Object{desiredCM("cfg", map[string]any{"k": "changed"})})
		if err != nil {
			t.Fatalf("Plan with force: %v", err)
		}
		if len(diffs) != 1 || diffs[0].Op != driver.OpUpdate {
			t.Fatalf("diffs = %+v, want one update", diffs)
		}
	})
}

// TestPlan_NoPrune pins --no-prune in both directions: with prune off
// the plan has no delete diff and apply leaves the stale object alive;
// with prune on, TestPlan_Prune above shows deletes still happen.
func TestPlan_NoPrune(t *testing.T) {
	ctx := context.Background()
	seeds := func() []runtime.Object {
		return []runtime.Object{
			seedNamespace(),
			appliedCM("keep", testOwner, map[string]any{"k": "v"}),
			appliedCM("stale", testOwner, map[string]any{"k": "w"}),
		}
	}
	desired := []driver.Object{desiredCM("keep", map[string]any{"k": "v"})}

	t.Run("plan has no delete diff", func(t *testing.T) {
		d, _ := newTestDriverOpts(t, false, false, nil, seeds()...)
		diffs, err := d.Plan(ctx, testOwner, desired)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		keep := false
		for _, df := range diffs {
			if df.Op == driver.OpDelete {
				t.Fatalf("prune-off plan produced a delete diff: %+v", df)
			}
			if df.Object.Name == "keep" && df.Op == driver.OpNone {
				keep = true
			}
		}
		if !keep {
			t.Errorf("diffs = %+v, want keep as none", diffs)
		}
	})

	t.Run("apply leaves the stale object alive", func(t *testing.T) {
		d, dyn := newTestDriverOpts(t, false, false, nil, seeds()...)
		diffs, err := d.Plan(ctx, testOwner, desired)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if err := d.Apply(ctx, testOwner, diffs); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if _, err := dyn.Tracker().Get(cmGVR, testNS, "stale", metav1.GetOptions{}); err != nil {
			t.Errorf("stale object was touched: %v", err)
		}
	})
}

// appliedSvc is the Service twin of appliedCM.
func appliedSvc(name string, owner driver.Owner) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"name": name, "namespace": testNS,
			"labels": map[string]any{flowLabel: owner.Flow, nodeLabel: owner.Node},
		},
		"spec": map[string]any{"type": "ClusterIP"},
	}}
}

// --- Apply ---------------------------------------------------------------

// mutationSeq records what actually wrote: non-dry-run patches and
// deletes, in order.
func mutationSeq(t *testing.T, dyn *fake.FakeDynamicClient) []string {
	t.Helper()
	var seq []string
	for _, a := range dyn.Actions() {
		switch act := a.(type) {
		case k8stesting.PatchActionImpl:
			if act.GetPatchType() == types.ApplyPatchType && len(act.PatchOptions.DryRun) == 0 {
				seq = append(seq, fmt.Sprintf("patch %s/%s", act.GetResource().Resource, act.GetName()))
			}
		case k8stesting.DeleteActionImpl:
			seq = append(seq, fmt.Sprintf("delete %s/%s", act.GetResource().Resource, act.GetName()))
		}
	}
	return seq
}

func TestApply_Ordering(t *testing.T) {
	ctx := context.Background()
	d, dyn := newTestDriver(t, false, nil, seedNamespace(),
		appliedCM("old", testOwner, map[string]any{"k": "v"}))

	err := d.Apply(ctx, testOwner, []driver.Diff{
		{Object: desiredDeploy("web", "nginx:1.28"), Op: driver.OpCreate},
		{Object: desiredCM("cfg", map[string]any{"k": "v"}), Op: driver.OpCreate},
		{Object: desiredSvc("web"), Op: driver.OpCreate},
		{Object: desiredCM("old", nil), Op: driver.OpDelete},
		{Object: desiredCM("same", nil), Op: driver.OpNone},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := mutationSeq(t, dyn)
	want := []string{
		"patch configmaps/cfg",  // tier 10
		"patch services/web",    // tier 30
		"patch deployments/web", // tier 40
		"delete configmaps/old", // deletes last
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("mutation order = %v, want %v", got, want)
	}
}

func TestApply_ValidateFirst(t *testing.T) {
	ctx := context.Background()
	d, dyn := newTestDriver(t, false, map[string]string{"b": "kubectl"},
		seedNamespace(),
		appliedCM("a", testOwner, map[string]any{"k": "1"}),
		appliedCM("b", testOwner, map[string]any{"k": "1"}))

	err := d.Apply(ctx, testOwner, []driver.Diff{
		{Object: desiredCM("a", map[string]any{"k": "2"}), Op: driver.OpUpdate},
		{Object: desiredCM("b", map[string]any{"k": "2"}), Op: driver.OpUpdate},
	})
	var ce *driver.ErrConflict
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if seq := mutationSeq(t, dyn); len(seq) != 0 {
		t.Errorf("conflict on the second object still wrote: %v", seq)
	}
}

func TestApply_ForceTakesOwnership(t *testing.T) {
	ctx := context.Background()
	d, dyn := newTestDriver(t, true, map[string]string{"cfg": "kubectl"},
		seedNamespace(), appliedCM("cfg", testOwner, map[string]any{"k": "old"}))

	if err := d.Apply(ctx, testOwner, []driver.Diff{
		{Object: desiredCM("cfg", map[string]any{"k": "new"}), Op: driver.OpUpdate},
	}); err != nil {
		t.Fatalf("Apply with force: %v", err)
	}
	if seq := mutationSeq(t, dyn); fmt.Sprint(seq) != fmt.Sprint([]string{"patch configmaps/cfg"}) {
		t.Errorf("mutations = %v, want the forced patch", seq)
	}
}

func TestApply_DeleteAbsent(t *testing.T) {
	ctx := context.Background()
	d, dyn := newTestDriver(t, false, nil, seedNamespace(),
		appliedCM("keep", testOwner, nil))

	err := d.Apply(ctx, testOwner, []driver.Diff{
		{Object: desiredCM("ghost", nil), Op: driver.OpDelete},
	})
	if err == nil || !contains(err.Error(), "absent") {
		t.Fatalf("err = %v, want absent-object error", err)
	}
	if seq := mutationSeq(t, dyn); len(seq) != 0 {
		t.Errorf("validation failed but something wrote: %v", seq)
	}
}

func TestApply_DeletePinsUID(t *testing.T) {
	ctx := context.Background()
	seed := appliedCM("old", testOwner, map[string]any{"k": "v"})
	seed.SetUID(types.UID("uid-old"))
	d, dyn := newTestDriver(t, false, nil, seedNamespace(), seed)

	if err := d.Apply(ctx, testOwner, []driver.Diff{
		{Object: desiredCM("old", nil), Op: driver.OpDelete},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	found := false
	for _, a := range dyn.Actions() {
		act, ok := a.(k8stesting.DeleteActionImpl)
		if !ok || act.GetName() != "old" {
			continue
		}
		found = true
		pre := act.DeleteOptions.Preconditions
		if pre == nil || pre.UID == nil || *pre.UID != types.UID("uid-old") {
			t.Errorf("delete preconditions = %+v, want UID uid-old", pre)
		}
	}
	if !found {
		t.Fatal("no delete action recorded")
	}
}

func TestApply_SkipsNoneAndRejectsUnknownOp(t *testing.T) {
	ctx := context.Background()
	d, dyn := newTestDriver(t, false, nil, seedNamespace())
	if err := d.Apply(ctx, testOwner, []driver.Diff{
		{Object: desiredCM("cfg", nil), Op: driver.OpNone},
	}); err != nil {
		t.Fatalf("Apply(none): %v", err)
	}
	if seq := mutationSeq(t, dyn); len(seq) != 0 {
		t.Errorf("none diff wrote: %v", seq)
	}
	if err := d.Apply(ctx, testOwner, []driver.Diff{
		{Object: desiredCM("cfg", nil), Op: driver.Op("bogus")},
	}); err == nil || !contains(err.Error(), "unsupported op") {
		t.Fatalf("err = %v, want unsupported-op error", err)
	}
}

// --- pure helpers --------------------------------------------------------

func TestApplyBody(t *testing.T) {
	d := &Driver{mapper: testMapper(), namespace: testNS}

	t.Run("stamps identity namespace and labels", func(t *testing.T) {
		body, err := d.applyBody(testOwner, desiredCM("cfg", map[string]any{"k": "v"}))
		if err != nil {
			t.Fatalf("applyBody: %v", err)
		}
		if body["apiVersion"] != "v1" || body["kind"] != "ConfigMap" {
			t.Errorf("identity = %v/%v", body["apiVersion"], body["kind"])
		}
		meta := body["metadata"].(map[string]any)
		if meta["name"] != "cfg" || meta["namespace"] != testNS {
			t.Errorf("metadata = %v", meta)
		}
		labels := meta["labels"].(map[string]any)
		if labels[flowLabel] != testFlow || labels[nodeLabel] != testNode {
			t.Errorf("labels = %v", labels)
		}
	})

	t.Run("nil data still builds a valid body", func(t *testing.T) {
		o := desiredCM("cfg", nil)
		o.Data = nil
		body, err := d.applyBody(testOwner, o)
		if err != nil {
			t.Fatalf("applyBody: %v", err)
		}
		if body["kind"] != "ConfigMap" {
			t.Errorf("body = %v", body)
		}
	})

	t.Run("namespace disagreement is an error", func(t *testing.T) {
		o := desiredCM("cfg", nil)
		o.Namespace = "other-ns"
		if _, err := d.applyBody(testOwner, o); err == nil || !contains(err.Error(), "manages") {
			t.Errorf("err = %v, want namespace disagreement", err)
		}
	})

	t.Run("contradicting data is an error", func(t *testing.T) {
		o := desiredCM("cfg", nil)
		o.Data["kind"] = "Secret"
		if _, err := d.applyBody(testOwner, o); err == nil || !contains(err.Error(), "contradicts") {
			t.Errorf("err = %v, want contradiction error", err)
		}
	})

	t.Run("missing name is an error", func(t *testing.T) {
		o := desiredCM("", nil)
		if _, err := d.applyBody(testOwner, o); err == nil || !contains(err.Error(), "no name") {
			t.Errorf("err = %v, want no-name error", err)
		}
	})
}

func TestStripVolatile(t *testing.T) {
	in := map[string]any{
		"metadata": map[string]any{
			"name":            "cfg",
			"resourceVersion": "9",
			"uid":             "abc",
			"annotations": map[string]any{
				"keep":          "yes",
				lastAppliedAnno: "{}",
			},
		},
		"status": map[string]any{"phase": "Running"},
		"data":   map[string]any{"k": "v"},
	}
	out := stripVolatile(in).(map[string]any)
	meta := out["metadata"].(map[string]any)
	if _, ok := meta["resourceVersion"]; ok {
		t.Error("resourceVersion survived")
	}
	if _, ok := out["status"]; ok {
		t.Error("status survived")
	}
	annos := meta["annotations"].(map[string]any)
	if _, ok := annos[lastAppliedAnno]; ok {
		t.Error("last-applied annotation survived")
	}
	if annos["keep"] != "yes" {
		t.Error("ordinary annotation stripped")
	}
	if out["data"].(map[string]any)["k"] != "v" {
		t.Error("data damaged")
	}
}

func TestSameState(t *testing.T) {
	live := map[string]any{
		"metadata": map[string]any{"name": "cfg", "resourceVersion": "1"},
		"spec":     map[string]any{"replicas": float64(1)},
		"status":   map[string]any{"phase": "Running"},
	}
	same := map[string]any{
		"metadata": map[string]any{"name": "cfg"},
		"spec":     map[string]any{"replicas": 1}, // int vs float64
	}
	if !sameState(live, same) {
		t.Error("bookkeeping and 1 vs 1.0 must compare equal")
	}
	changed := map[string]any{
		"metadata": map[string]any{"name": "cfg"},
		"spec":     map[string]any{"replicas": 2},
	}
	if sameState(live, changed) {
		t.Error("real change classified equal")
	}
}

func TestAsConflict(t *testing.T) {
	o := driver.Object{Kind: "ConfigMap", Name: "cfg"}
	multi := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: "Failure", Reason: metav1.StatusReasonConflict,
		Message: `Apply failed with 2 conflicts: conflict with "a": .metadata.labels.x, conflict with "b": .spec.thing, conflict with "a": .other`,
	}}
	err := asConflict(o, multi)
	var ce *driver.ErrConflict
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if fmt.Sprint(ce.Managers) != fmt.Sprint([]string{"a", "b"}) {
		t.Errorf("Managers = %v, want [a b] (deduped, ordered)", ce.Managers)
	}

	// Optimistic-concurrency 409: no field-manager pattern → not ours.
	optimistic := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: "Failure", Reason: metav1.StatusReasonConflict,
		Message: `Operation cannot be fulfilled on configmaps "cfg": the object has been modified`,
	}}
	if got := asConflict(o, optimistic); got != nil {
		t.Errorf("optimistic conflict = %v, want nil", got)
	}
	if got := asConflict(o, nil); got != nil {
		t.Errorf("nil error = %v, want nil", got)
	}
	plain := fmt.Errorf("boom")
	if got := asConflict(o, plain); got != nil {
		t.Errorf("non-conflict = %v, want nil", got)
	}
}

func TestSortDiffs(t *testing.T) {
	d := func(op driver.Op, kind, name string) driver.Diff {
		return driver.Diff{Op: op, Object: driver.Object{Kind: kind, Name: name}}
	}
	diffs := []driver.Diff{
		d(driver.OpDelete, "ConfigMap", "z-old"),
		d(driver.OpCreate, "Deployment", "web"),
		d(driver.OpCreate, "Ingress", "web"),
		d(driver.OpCreate, "ConfigMap", "a"),
		d(driver.OpCreate, "Service", "web"),
		d(driver.OpDelete, "Deployment", "x-old"),
		d(driver.OpNone, "Secret", "s"),
	}
	sortDiffs(diffs)
	got := make([]string, len(diffs))
	for i, df := range diffs {
		got[i] = string(df.Op) + " " + df.Object.Kind + "/" + df.Object.Name
	}
	want := []string{
		"create ConfigMap/a",
		"none Secret/s", // tier 10 alongside ConfigMap, kind order
		"create Service/web",
		"create Deployment/web",
		"create Ingress/web",
		"delete Deployment/x-old", // deletes last, reverse tier
		"delete ConfigMap/z-old",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("order =\n %v\nwant\n %v", got, want)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
