package probe

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestPolicyTypes(t *testing.T) {
	np := &networkingv1.NetworkPolicy{
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress,
			},
		},
	}
	got := policyTypes(np)
	if len(got) != 2 || got[0] != "Ingress" || got[1] != "Egress" {
		t.Errorf("policyTypes = %v", got)
	}
	if policyTypes(&networkingv1.NetworkPolicy{}) != nil {
		t.Error("empty policy types should be nil")
	}
}

func TestUnstructuredHelpers(t *testing.T) {
	gc := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"controller": "istio.io/gateway-controller"},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Accepted", "status": "True"},
				map[string]any{"status": "ignored"}, // no type: skipped
			},
		},
	}}
	if got := unstructuredString(gc, "spec", "controller"); got != "istio.io/gateway-controller" {
		t.Errorf("unstructuredString = %q", got)
	}
	if got := unstructuredString(gc, "spec", "missing"); got != "" {
		t.Errorf("missing field = %q, want empty", got)
	}
	if got := conditionStatus(gc, "Accepted"); got != "True" {
		t.Errorf("conditionStatus = %q, want True", got)
	}
	if got := conditionStatus(gc, "Ready"); got != "" {
		t.Errorf("absent condition = %q, want empty", got)
	}
	bare := &unstructured.Unstructured{Object: map[string]any{}}
	if got := conditionStatus(bare, "Accepted"); got != "" {
		t.Errorf("no conditions = %q, want empty", got)
	}
}

func TestCNINHints(t *testing.T) {
	client := fake.NewClientset()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "n1",
		Annotations: map[string]string{
			"flannel.alpha.coreos.com/public-ip": "10.0.0.5",
		},
	}}
	if _, err := client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cilium-agent", "flannel-ds", "kube-proxy"} {
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"}}
		if _, err := client.AppsV1().DaemonSets("kube-system").Create(context.Background(), ds, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	p := &prober{in: Input{Client: client}}
	hints, errs := p.cniHints(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	// flannel found from both sources but reported once; sorted.
	if len(hints) != 2 || hints[0] != "cilium" || hints[1] != "flannel" {
		t.Errorf("hints = %v, want [cilium flannel]", hints)
	}
}

func TestCNINodesForbidden(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "nodes",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "cilium-agent", Namespace: "kube-system"}}
	if _, err := client.AppsV1().DaemonSets("kube-system").Create(context.Background(), ds, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := &prober{in: Input{Client: client}}
	hints, errs := p.cniHints(context.Background())
	if len(errs) != 1 || errs[0].Resource != "nodes" || errs[0].Code != "Forbidden" {
		t.Fatalf("errs = %+v", errs)
	}
	if len(hints) != 1 || hints[0] != "cilium" {
		t.Errorf("hints = %v, want daemonset-derived cilium", hints)
	}
}

// seedGatewayDiscovery registers the gateway API group with the fake
// discovery so ServerGroups reports it at v1.
func seedGatewayDiscovery(client *fake.Clientset) {
	fd := client.Discovery().(*dfake.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{
		{GroupVersion: "gateway.networking.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "gatewayclasses", Kind: "GatewayClass"},
		}},
	}
}

func TestProbeGatewayAPIAbsent(t *testing.T) {
	client := fake.NewClientset()
	fd := client.Discovery().(*dfake.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod"}}},
	}
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	gw, errs := p.probeGatewayAPI(context.Background())
	if gw != nil {
		t.Errorf("gw = %+v, want nil (no CRDs is a fact, not an error)", gw)
	}
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
}

func TestProbeGatewayAPIPresentNoDynamic(t *testing.T) {
	client := fake.NewClientset()
	seedGatewayDiscovery(client)
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	gw, errs := p.probeGatewayAPI(context.Background())
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want 1", errs)
	}
	got := errs[0]
	if got.Section != "networking" || got.Resource != "gateway.networking.k8s.io" ||
		got.Code != "Error" || got.Reason != "dynamic client not configured" {
		t.Errorf("ProbeError = %+v", got)
	}
	if gw == nil || !gw.Present || len(gw.Versions) != 1 || gw.Versions[0] != "v1" {
		t.Errorf("gw = %+v, want present with version info only", gw)
	}
}

func TestProbeGatewayAPIServerGroupsForbidden(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("get", "group",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	gw, errs := p.probeGatewayAPI(context.Background())
	if gw != nil {
		t.Errorf("gw = %+v, want nil", gw)
	}
	if len(errs) != 1 || errs[0].Resource != "servergroups" || errs[0].Code != "Forbidden" {
		t.Errorf("errs = %+v", errs)
	}
}

func uObj(apiVersion, kind, name, namespace string, content map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}
	if namespace != "" {
		obj["metadata"].(map[string]any)["namespace"] = namespace
	}
	for k, v := range content {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

// TestProbeGatewayAPIFull: classes, gateways and httproutes come through the
// dynamic client with their status/spec fields extracted and sorted.
func TestProbeGatewayAPIFull(t *testing.T) {
	client := fake.NewClientset()
	seedGatewayDiscovery(client)

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"}: "GatewayClassList",
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}:       "GatewayList",
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}:     "HTTPRouteList",
	}
	// Objects must be created through the client: passing them to the
	// constructor stores them under the UnsafeGuessKindToResource plural
	// ("gatewaies"), which never matches the list GVR.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind)
	gcGVR := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"}
	gtwGVR := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	rtGVR := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

	if _, err := dyn.Resource(gcGVR).Create(context.Background(),
		uObj("gateway.networking.k8s.io/v1", "GatewayClass", "gc-b", "", map[string]any{
			"spec": map[string]any{"controller": "b.io/gw"},
		}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(gcGVR).Create(context.Background(),
		uObj("gateway.networking.k8s.io/v1", "GatewayClass", "gc-a", "", map[string]any{
			"spec": map[string]any{"controller": "a.io/gw"},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Accepted", "status": "True"}},
			},
		}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(gtwGVR).Namespace("team-a").Create(context.Background(),
		uObj("gateway.networking.k8s.io/v1", "Gateway", "gw-1", "team-a", map[string]any{
			"spec": map[string]any{
				"listeners": []any{map[string]any{"name": "http"}, map[string]any{"name": "https"}},
			},
			"status": map[string]any{
				"addresses": []any{map[string]any{"value": "10.0.0.9"}},
			},
		}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(rtGVR).Namespace("team-a").Create(context.Background(),
		uObj("gateway.networking.k8s.io/v1", "HTTPRoute", "route-2", "team-a", map[string]any{
			"spec": map[string]any{
				"hostnames": []any{"b.example.com"},
			},
		}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(rtGVR).Namespace("team-a").Create(context.Background(),
		uObj("gateway.networking.k8s.io/v1", "HTTPRoute", "route-1", "team-a", map[string]any{
			"spec": map[string]any{
				"hostnames":  []any{"a.example.com"},
				"parentRefs": []any{map[string]any{"name": "gw-1"}},
			},
		}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	p := &prober{in: Input{Client: client, Dynamic: dyn, Namespace: "team-a"}}
	gw, errs := p.probeGatewayAPI(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if gw == nil || !gw.Present {
		t.Fatalf("gw = %+v, want present", gw)
	}
	if len(gw.Versions) != 1 || gw.Versions[0] != "v1" {
		t.Errorf("versions = %v", gw.Versions)
	}
	if len(gw.Classes) != 2 || gw.Classes[0].Name != "gc-a" || gw.Classes[1].Name != "gc-b" {
		t.Fatalf("classes = %+v, want sorted [gc-a gc-b]", gw.Classes)
	}
	if gw.Classes[0].Controller != "a.io/gw" || gw.Classes[0].Accepted != "True" {
		t.Errorf("gc-a = %+v", gw.Classes[0])
	}
	if gw.Classes[1].Accepted != "" {
		t.Errorf("gc-b Accepted = %q, want empty", gw.Classes[1].Accepted)
	}
	if len(gw.Gateways) != 1 {
		t.Fatalf("gateways = %+v", gw.Gateways)
	}
	g := gw.Gateways[0]
	if g.Name != "gw-1" || g.Listeners != 2 || len(g.Addresses) != 1 || g.Addresses[0] != "10.0.0.9" {
		t.Errorf("gateway = %+v", g)
	}
	if len(gw.HTTPRoutes) != 2 || gw.HTTPRoutes[0].Name != "route-1" || gw.HTTPRoutes[1].Name != "route-2" {
		t.Fatalf("httproutes = %+v, want sorted", gw.HTTPRoutes)
	}
	r1 := gw.HTTPRoutes[0]
	if len(r1.Hostnames) != 1 || r1.Hostnames[0] != "a.example.com" ||
		len(r1.Gateways) != 1 || r1.Gateways[0] != "gw-1" {
		t.Errorf("route-1 = %+v", r1)
	}
}

func TestProbeNetworkingSuccess(t *testing.T) {
	client := fake.NewClientset()
	fd := client.Discovery().(*dfake.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod"}}},
	}
	for _, ic := range []networkingv1.IngressClass{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "zeta"},
			Spec:       networkingv1.IngressClassSpec{Controller: "z.io/ingress"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "alpha",
				Annotations: map[string]string{
					"ingressclass.kubernetes.io/is-default-class": "true",
				},
			},
			Spec: networkingv1.IngressClassSpec{Controller: "a.io/ingress"},
		},
	} {
		ic := ic
		if _, err := client.NetworkingV1().IngressClasses().Create(context.Background(), &ic, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-all", Namespace: "team-a"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{}, {}},
		},
	}
	if _, err := client.NetworkingV1().NetworkPolicies("team-a").Create(context.Background(), np, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "n1",
		Annotations: map[string]string{"flannel.alpha.coreos.com/public-ip": "10.0.0.5"},
	}}
	if _, err := client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeNetworking(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if info.GatewayAPI != nil {
		t.Errorf("GatewayAPI = %+v, want nil without CRDs", info.GatewayAPI)
	}
	if len(info.IngressClasses) != 2 ||
		info.IngressClasses[0].Name != "alpha" || !info.IngressClasses[0].IsDefault ||
		info.IngressClasses[1].Name != "zeta" || info.IngressClasses[1].IsDefault {
		t.Fatalf("ingress classes = %+v", info.IngressClasses)
	}
	if len(info.NetworkPolicies) != 1 {
		t.Fatalf("netpols = %+v", info.NetworkPolicies)
	}
	npInfo := info.NetworkPolicies[0]
	if npInfo.Name != "deny-all" ||
		npInfo.PodSelector["app"] != "web" ||
		len(npInfo.PolicyTypes) != 1 || npInfo.PolicyTypes[0] != "Ingress" ||
		npInfo.IngressRules != 1 || npInfo.EgressRules != 2 {
		t.Errorf("netpol = %+v", npInfo)
	}
	if len(info.CNIHints) != 1 || info.CNIHints[0] != "flannel" {
		t.Errorf("CNIHints = %v", info.CNIHints)
	}
	if st := classify(info, errs); st.Status != StatusOK {
		t.Errorf("classify = %+v, want ok", st)
	}
}

// TestProbeNetworkingDenied: err order is ingressclasses → gateway → cniHints
// → networkpolicies regardless of which sub-probe failed.
func TestProbeNetworkingDenied(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "ingressclasses",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	client.PrependReactor("list", "networkpolicies",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	client.PrependReactor("list", "nodes",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeNetworking(context.Background())
	if len(errs) != 3 {
		t.Fatalf("errs = %+v, want 3", errs)
	}
	wantResources := []string{"ingressclasses", "nodes", "networkpolicies"}
	for i, want := range wantResources {
		if errs[i].Resource != want || errs[i].Code != "Forbidden" {
			t.Errorf("errs[%d] = %+v, want %s forbidden", i, errs[i], want)
		}
	}
	if info.IngressClasses != nil || info.NetworkPolicies != nil || info.CNIHints != nil {
		t.Errorf("denied lists must stay nil: %+v", info)
	}
}

// TestProbeNetworkingNetPolNamespaceMissing: a NotFound from the namespaced
// netpol list is an empty fact, not an error.
func TestProbeNetworkingNetPolNamespaceMissing(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "networkpolicies",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "")
		})
	p := &prober{in: Input{Client: client, Namespace: "ghost"}}
	info, errs := p.probeNetworking(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none for NotFound", errs)
	}
	// gentype's fake List returns a non-nil empty list alongside the
	// NotFound, so the section records "probed and none", not "not probed".
	if info.NetworkPolicies == nil || len(info.NetworkPolicies) != 0 {
		t.Errorf("NetworkPolicies = %v, want empty slice", info.NetworkPolicies)
	}
}
