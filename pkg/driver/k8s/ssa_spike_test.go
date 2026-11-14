package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// Stage 2a spike. Five questions against a live API server:
//
//  1. does strip(dryRunApply(desired)) == strip(liveGET) classify an
//     unchanged object as none (the kubectl-diff algorithm)?
//  2. is dry-run deterministic across consecutive calls?
//  3. what does a cross-manager 409 look like, and does dry-run surface it?
//  4. does a label selector scope prune listing to one owner?
//  5. does re-applying a field whose value matches the server default
//     classify as none or update?
//
// The test mutates only its own afon-spike namespace and never runs
// against anything but a local sandbox context.

// The spike shares the package's production fixtures where they exist:
// nsGVR, ssaPatch, objName/objNamespace, and the volatile strip list
// live in client.go and diff.go — the spike validates the very code
// the driver now uses.

const (
	spikeNS    = "afon-spike"
	spikeFlow  = "spike"
	spikeNode  = "n1"
	spikeLabel = "afon.dev/flow=spike,afon.dev/node=n1"
)

// spikeContextAllowed is the safety gate: a test that SSA-applies into a
// cluster must never ride the ambient current context. Local sandboxes
// only (IsLocalSandbox), unless AFON_SPIKE_ALLOW_CONTEXT names the
// context explicitly — the env escape hatch stays test-side.
func spikeContextAllowed(name string) bool {
	if name == "" {
		return false
	}
	if env := os.Getenv("AFON_SPIKE_ALLOW_CONTEXT"); env != "" && env == name {
		return true
	}
	return IsLocalSandbox(name)
}

// spikeDynamicClient gates the whole spike: log the context, allowlist
// it, ping discovery with a short timeout. Down or disallowed → skip,
// never fail (CI without a cluster stays green).
func spikeDynamicClient(t *testing.T) dynamic.Interface {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	raw, err := cc.RawConfig()
	if err != nil {
		t.Skipf("kubeconfig unavailable: %v", err)
	}
	name := raw.CurrentContext
	t.Logf("spike context: %q", name)
	if !spikeContextAllowed(name) {
		t.Skipf("context %q is not a local sandbox (allowlist: k3d-*, kind-*, minikube, rancher-desktop, docker-desktop); set AFON_SPIKE_ALLOW_CONTEXT=%s to run here", name, name)
	}
	restCfg, err := cc.ClientConfig()
	if err != nil {
		t.Skipf("rest config for %q: %v", name, err)
	}
	restCfg.Timeout = 30 * time.Second

	// Discovery ping with a short timeout: cluster down → skip, not fail.
	ping := *restCfg
	ping.Timeout = 2 * time.Second
	disco, err := discovery.NewDiscoveryClientForConfig(&ping)
	if err != nil {
		t.Skipf("discovery client for %q: %v", name, err)
	}
	if _, err := disco.ServerVersion(); err != nil {
		t.Skipf("cluster %q unreachable: %v", name, err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("dynamic client for %q: %v", name, err)
	}
	return dyn
}

// setupSpikeNamespace self-heals: deletes leftovers from an interrupted
// run (waiting out termination), then creates the namespace fresh.
// Cleanup deletes it with background propagation.
func setupSpikeNamespace(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	ctx := context.Background()
	nsRes := dyn.Resource(nsGVR)
	prop := metav1.DeletePropagationBackground
	if err := nsRes.Delete(ctx, spikeNS, metav1.DeleteOptions{PropagationPolicy: &prop}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting leftover namespace %q: %v", spikeNS, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := nsRes.Get(ctx, spikeNS, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			t.Fatalf("checking namespace %q: %v", spikeNS, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("namespace %q stuck terminating", spikeNS)
		}
		time.Sleep(200 * time.Millisecond)
	}
	nsObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": spikeNS},
	}}
	if _, err := nsRes.Create(ctx, nsObj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace %q: %v", spikeNS, err)
	}
	t.Cleanup(func() {
		if err := nsRes.Delete(context.Background(), spikeNS, metav1.DeleteOptions{PropagationPolicy: &prop}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: deleting namespace %q: %v", spikeNS, err)
		}
	})
}

func mustPatch(t *testing.T, ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource,
	obj map[string]any, manager string, force, dryRun bool) *unstructured.Unstructured {
	t.Helper()
	u, err := ssaPatch(ctx, dyn, gvr, obj, manager, force, dryRun)
	if err != nil {
		t.Fatalf("apply %s/%s (manager=%s force=%v dryRun=%v): %v",
			gvr.Resource, objName(obj), manager, force, dryRun, err)
	}
	return u
}

func getObj(t *testing.T, ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s/%s: %v", ns, gvr.Resource, name, err)
	}
	return u
}

// --- fixtures ------------------------------------------------------------

func afonLabels() map[string]any {
	return map[string]any{"afon.dev/flow": spikeFlow, "afon.dev/node": spikeNode}
}

// serviceObj builds a NodePort Service with no clusterIP/nodePort: the
// server assigns both — the classic false-positive source.
func serviceObj(ns, name string, labels map[string]any) map[string]any {
	meta := map[string]any{"name": name, "namespace": ns}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": meta,
		"spec": map[string]any{
			"type":  "NodePort",
			"ports": []any{map[string]any{"port": 80, "targetPort": 8080}},
		},
	}
}

// deployObj builds a Deployment. replicas == nil omits the field so the
// server defaults it (replicas=1, strategy, progressDeadlineSeconds, …);
// a non-nil value writes it explicitly.
func deployObj(ns, name string, labels map[string]any, image string, replicas any) map[string]any {
	meta := map[string]any{"name": name, "namespace": ns}
	if labels != nil {
		meta["labels"] = labels
	}
	spec := map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": name}},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "app", "image": image}},
			},
		},
	}
	if replicas != nil {
		spec["replicas"] = replicas
	}
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta, "spec": spec,
	}
}

// --- strip + compare -----------------------------------------------------

// canonicalJSON strips (the production strip list in diff.go) then
// marshals; encoding/json sorts map keys, so equal states produce
// byte-identical output.
func canonicalJSON(t *testing.T, obj map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(stripVolatile(obj))
	if err != nil {
		t.Fatalf("canonical marshal: %v", err)
	}
	return b
}

// diffPaths lists where two (already stripped) documents differ — the
// strip-list ratification output when a comparison fails.
func diffPaths(a, b any, path string, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: type %T != %T", path, a, b))
			return
		}
		for _, k := range unionKeys(av, bv) {
			av2, aok := av[k]
			bv2, bok := bv[k]
			p := path + "." + k
			switch {
			case !aok:
				*out = append(*out, fmt.Sprintf("%s: only in b: %v", p, bv2))
			case !bok:
				*out = append(*out, fmt.Sprintf("%s: only in a: %v", p, av2))
			default:
				diffPaths(av2, bv2, p, out)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: type %T != %T", path, a, b))
			return
		}
		if len(av) != len(bv) {
			*out = append(*out, fmt.Sprintf("%s: length %d != %d", path, len(av), len(bv)))
			return
		}
		for i := range av {
			diffPaths(av[i], bv[i], fmt.Sprintf("%s[%d]", path, i), out)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, fmt.Sprintf("%s: %v != %v", path, a, b))
		}
	}
}

func unionKeys(a, b map[string]any) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// assertSameState fails with the differing paths when dry-run and live
// disagree after stripping — exactly the report needed to extend the
// strip list.
func assertSameState(t *testing.T, label string, dryRunObj, liveObj map[string]any) {
	t.Helper()
	if bytes.Equal(canonicalJSON(t, dryRunObj), canonicalJSON(t, liveObj)) {
		return
	}
	var paths []string
	diffPaths(stripVolatile(dryRunObj), stripVolatile(liveObj), "", &paths)
	t.Errorf("%s: dry-run != live after strip; differing paths:\n  %s",
		label, strings.Join(paths, "\n  "))
}

func listNames(t *testing.T, ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, selector string) []string {
	t.Helper()
	list, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("list %s in %s (selector %q): %v", gvr.Resource, ns, selector, err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	slices.Sort(names)
	return names
}

// --- the spike ------------------------------------------------------------

func TestSpike(t *testing.T) {
	ctx := context.Background()
	dyn := spikeDynamicClient(t)
	setupSpikeNamespace(t, dyn)

	svc := serviceObj(spikeNS, "spike-svc", afonLabels())
	deploy := deployObj(spikeNS, "spike-deploy", afonLabels(), "nginx:1.27", nil)

	t.Run("NoOpClassification", func(t *testing.T) {
		mustPatch(t, ctx, dyn, svcGVR, svc, "afon", false, false)
		mustPatch(t, ctx, dyn, deployGVR, deploy, "afon", false, false)

		// Ownership labels landed — prune listing depends on them.
		liveSvc := getObj(t, ctx, dyn, svcGVR, spikeNS, "spike-svc")
		if got, _, _ := unstructured.NestedString(liveSvc.Object, "metadata", "labels", "afon.dev/flow"); got != spikeFlow {
			t.Errorf("service afon.dev/flow label = %q, want %q", got, spikeFlow)
		}

		// (2) Dry-run determinism: two consecutive dry-runs of the same
		// desired state must be byte-identical after strip, or no-op
		// classification would fail intermittently in 2b.
		for _, tc := range []struct {
			gvr  schema.GroupVersionResource
			obj  map[string]any
			name string
		}{{svcGVR, svc, "service"}, {deployGVR, deploy, "deployment"}} {
			dr1 := mustPatch(t, ctx, dyn, tc.gvr, tc.obj, "afon", false, true)
			dr2 := mustPatch(t, ctx, dyn, tc.gvr, tc.obj, "afon", false, true)
			b1, b2 := canonicalJSON(t, dr1.Object), canonicalJSON(t, dr2.Object)
			if !bytes.Equal(b1, b2) {
				var paths []string
				diffPaths(stripVolatile(dr1.Object), stripVolatile(dr2.Object), "", &paths)
				t.Errorf("dry-run not deterministic for %s:\n  %s", tc.name, strings.Join(paths, "\n  "))
			}
		}

		// (1) No-op classification: strip(dryRun(desired)) == strip(live)
		// for unchanged objects — Service with assigned clusterIP/nodePort
		// and Deployment with server-defaulted spec.
		liveSvc = getObj(t, ctx, dyn, svcGVR, spikeNS, "spike-svc")
		drySvc := mustPatch(t, ctx, dyn, svcGVR, svc, "afon", false, true)
		assertSameState(t, "service unchanged", drySvc.Object, liveSvc.Object)

		liveDeploy := getObj(t, ctx, dyn, deployGVR, spikeNS, "spike-deploy")
		dryDeploy := mustPatch(t, ctx, dyn, deployGVR, deploy, "afon", false, true)
		assertSameState(t, "deployment unchanged", dryDeploy.Object, liveDeploy.Object)

		// Update classification: mutate the image → dry-run differs.
		deployMutated := deployObj(spikeNS, "spike-deploy", afonLabels(), "nginx:1.28", nil)
		dryMut := mustPatch(t, ctx, dyn, deployGVR, deployMutated, "afon", false, true)
		liveDeploy = getObj(t, ctx, dyn, deployGVR, spikeNS, "spike-deploy")
		if bytes.Equal(canonicalJSON(t, dryMut.Object), canonicalJSON(t, liveDeploy.Object)) {
			t.Fatal("image mutation not detected: dry-run equals live")
		}
		// Apply the mutation → back to none.
		mustPatch(t, ctx, dyn, deployGVR, deployMutated, "afon", false, false)
		liveDeploy = getObj(t, ctx, dyn, deployGVR, spikeNS, "spike-deploy")
		dryMut = mustPatch(t, ctx, dyn, deployGVR, deployMutated, "afon", false, true)
		assertSameState(t, "deployment after update applied", dryMut.Object, liveDeploy.Object)

		// (5) Field added whose value already matches the server default:
		// live has replicas=1 (defaulted); re-apply with replicas: 1
		// written explicitly. Classification is observational — either
		// answer is acceptable, but 2b must know which.
		deployExplicit := deployObj(spikeNS, "spike-deploy", afonLabels(), "nginx:1.28", 1)
		dryExplicit := mustPatch(t, ctx, dyn, deployGVR, deployExplicit, "afon", false, true)
		liveDeploy = getObj(t, ctx, dyn, deployGVR, spikeNS, "spike-deploy")
		class := "update"
		if bytes.Equal(canonicalJSON(t, dryExplicit.Object), canonicalJSON(t, liveDeploy.Object)) {
			class = "none"
		}
		t.Logf("SPIKE-RESULT(defaulted-field-reapply): classified %s", class)
	})

	t.Run("Conflict", func(t *testing.T) {
		// (a) Label conflict: spike-other owns labels.team.
		cmOther := map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "spike-cm", "namespace": spikeNS,
				"labels": map[string]any{"team": "other"}},
			"data": map[string]any{"mode": "other"},
		}
		mustPatch(t, ctx, dyn, cmGVR, cmOther, "spike-other", false, false)
		cmAfon := map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "spike-cm", "namespace": spikeNS,
				"labels": map[string]any{"team": "afon", "afon.dev/flow": spikeFlow, "afon.dev/node": spikeNode}},
		}
		// Does dryRun=All with force=false surface the conflict?
		if _, err := ssaPatch(ctx, dyn, cmGVR, cmAfon, "afon", false, true); err != nil {
			t.Logf("SPIKE-RESULT(conflict-dry-run-label): surfaces conflict: %v", err)
		} else {
			t.Logf("SPIKE-RESULT(conflict-dry-run-label): no error — plan cannot detect this conflict")
		}
		// Real apply, force=false: must 409; capture the Status message
		// the 2b parser will target.
		if _, err := ssaPatch(ctx, dyn, cmGVR, cmAfon, "afon", false, false); err == nil {
			t.Fatal("expected conflict applying over spike-other's label with force=false")
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("expected apierrors.IsConflict, got %T: %v", err, err)
		} else {
			t.Logf("SPIKE-RESULT(conflict-real-apply-label): %v", err)
		}
		// force=true takes the field.
		mustPatch(t, ctx, dyn, cmGVR, cmAfon, "afon", true, false)
		liveCM := getObj(t, ctx, dyn, cmGVR, spikeNS, "spike-cm")
		if got, _, _ := unstructured.NestedString(liveCM.Object, "metadata", "labels", "team"); got != "afon" {
			t.Errorf("after force apply, labels.team = %q, want afon", got)
		}

		// (b) spec field conflict: spike-other owns replicas on its own
		// Deployment — the 409 shape for a non-label field.
		deployOther := deployObj(spikeNS, "spike-conflict-deploy",
			map[string]any{"owned-by": "other"}, "nginx:1.27", 2)
		mustPatch(t, ctx, dyn, deployGVR, deployOther, "spike-other", false, false)
		deployAfon := deployObj(spikeNS, "spike-conflict-deploy", afonLabels(), "nginx:1.27", 3)
		if _, err := ssaPatch(ctx, dyn, deployGVR, deployAfon, "afon", false, true); err != nil {
			t.Logf("SPIKE-RESULT(conflict-dry-run-replicas): surfaces conflict: %v", err)
		} else {
			t.Logf("SPIKE-RESULT(conflict-dry-run-replicas): no error — plan cannot detect this conflict")
		}
		if _, err := ssaPatch(ctx, dyn, deployGVR, deployAfon, "afon", false, false); err == nil {
			t.Fatal("expected conflict applying over spike-other's replicas with force=false")
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("expected apierrors.IsConflict, got %T: %v", err, err)
		} else {
			t.Logf("SPIKE-RESULT(conflict-real-apply-replicas): %v", err)
		}
		mustPatch(t, ctx, dyn, deployGVR, deployAfon, "afon", true, false)
		liveConf := getObj(t, ctx, dyn, deployGVR, spikeNS, "spike-conflict-deploy")
		if got, _, _ := unstructured.NestedInt64(liveConf.Object, "spec", "replicas"); got != 3 {
			t.Errorf("after force apply, spec.replicas = %d, want 3", got)
		}
	})

	t.Run("PruneSelector", func(t *testing.T) {
		// Another flow's Service: same namespace, different owner labels.
		foreign := serviceObj(spikeNS, "spike-foreign",
			map[string]any{"afon.dev/flow": "other", "afon.dev/node": "x"})
		mustPatch(t, ctx, dyn, svcGVR, foreign, "afon", false, false)

		// Owned objects, one selector: exactly this flow+node's objects,
		// foreign excluded, across every kind involved.
		if got, want := listNames(t, ctx, dyn, svcGVR, spikeNS, spikeLabel), []string{"spike-svc"}; !slices.Equal(got, want) {
			t.Errorf("services by owner = %v, want %v", got, want)
		}
		if got, want := listNames(t, ctx, dyn, deployGVR, spikeNS, spikeLabel),
			[]string{"spike-conflict-deploy", "spike-deploy"}; !slices.Equal(got, want) {
			t.Errorf("deployments by owner = %v, want %v", got, want)
		}
		if got := listNames(t, ctx, dyn, cmGVR, spikeNS, spikeLabel); len(got) != 1 || got[0] != "spike-cm" {
			t.Errorf("configmaps by owner = %v, want [spike-cm]", got)
		}
		// Foreign scoping: flow=other finds only the foreign object, and
		// it never leaked into the flow=spike lists above.
		if got, want := listNames(t, ctx, dyn, svcGVR, spikeNS, "afon.dev/flow=other"), []string{"spike-foreign"}; !slices.Equal(got, want) {
			t.Errorf("foreign services = %v, want %v", got, want)
		}
	})
}
