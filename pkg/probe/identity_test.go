package probe

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestProbeIdentitySuccess(t *testing.T) {
	client := fake.NewClientset()
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uid-123"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	p := &prober{in: Input{
		Client:      client,
		KubeContext: "prod",
		ClusterName: "prod-cluster",
		RESTConfig:  &rest.Config{Host: "https://10.0.0.9:6443"},
	}}
	id, errs := p.probeIdentity(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if id.KubeContext != "prod" || id.ClusterName != "prod-cluster" {
		t.Errorf("identity = %+v", id)
	}
	if id.Server != "https://10.0.0.9:6443" {
		t.Errorf("Server = %q", id.Server)
	}
	if id.ClusterUID != "uid-123" {
		t.Errorf("ClusterUID = %q, want uid-123", id.ClusterUID)
	}
}

// TestProbeIdentityNamespaceForbidden: the kube-system read failing only
// costs the cluster UID; context/cluster/server still come from Input.
func TestProbeIdentityNamespaceForbidden(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "kube-system", nil)
	client := fake.NewClientset()
	client.PrependReactor("get", "namespaces",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})
	p := &prober{in: Input{
		Client:      client,
		KubeContext: "prod",
		ClusterName: "prod-cluster",
		RESTConfig:  &rest.Config{Host: "https://10.0.0.9:6443"},
	}}
	id, errs := p.probeIdentity(context.Background())
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want 1", errs)
	}
	got := errs[0]
	if got.Section != "identity" || got.Resource != "namespaces/kube-system" ||
		got.Code != "Forbidden" || got.Reason != forbidden.Error() {
		t.Errorf("ProbeError = %+v", got)
	}
	if id.ClusterUID != "" {
		t.Errorf("ClusterUID = %q, want empty", id.ClusterUID)
	}
	if id.KubeContext != "prod" {
		t.Errorf("KubeContext = %q", id.KubeContext)
	}
}

// TestProbeIdentityNoRESTConfig: without a RESTConfig there is no server URL
// to report and the empty string must stay empty.
func TestProbeIdentityNoRESTConfig(t *testing.T) {
	client := fake.NewClientset()
	p := &prober{in: Input{Client: client}}
	id, _ := p.probeIdentity(context.Background())
	if id.Server != "" {
		t.Errorf("Server = %q, want empty", id.Server)
	}
}

func TestDetectFlavor(t *testing.T) {
	cases := []struct {
		name  string
		v     VersionInfo
		nodes []NodeInfo
		want  Flavor
	}{
		{
			name: "k3s suffix no nodes",
			v:    VersionInfo{Server: ServerVersionInfo{GitVersion: "v1.35.5+k3s1"}},
			want: Flavor{VersionSuffix: "k3s1", Name: "k3s"},
		},
		{
			name: "no suffix no nodes",
			v:    VersionInfo{Server: ServerVersionInfo{GitVersion: "v1.34.1"}},
			want: Flavor{},
		},
		{
			name: "unknown suffix passes through",
			v:    VersionInfo{Server: ServerVersionInfo{GitVersion: "v1.30.0+acme.1"}},
			want: Flavor{VersionSuffix: "acme.1", Name: "acme.1"},
		},
		{
			name:  "kind from providerID",
			nodes: []NodeInfo{{Name: "kind-control-plane", ProviderID: "kind://kind-control-plane"}},
			want:  Flavor{Name: "kind", ProviderID: "kind://kind-control-plane"},
		},
		{
			name:  "gce from provider scheme",
			nodes: []NodeInfo{{Name: "n1", ProviderID: "gce://proj/zone/inst"}},
			want:  Flavor{Name: "gce", ProviderID: "gce://proj/zone/inst"},
		},
		{
			name: "rke2 suffix",
			v:    VersionInfo{Server: ServerVersionInfo{GitVersion: "v1.31.1+rke2.1"}},
			want: Flavor{VersionSuffix: "rke2.1", Name: "rke2"},
		},
		{
			name: "cni hints from annotations are sorted",
			nodes: []NodeInfo{{
				Name: "n1",
				Annotations: map[string]string{
					"cilium.io/device":               "eth0",
					"projectcalico.org/ipv4-address": "10.0.0.1",
				},
			}},
			want: Flavor{Hints: []string{"calico", "cilium"}},
		},
		{
			name: "k3s marker annotation",
			nodes: []NodeInfo{{
				Name:        "n1",
				Annotations: map[string]string{"k3s.io/node-ip": "10.0.0.1"},
			}},
			want: Flavor{Hints: []string{"k3s"}},
		},
		{
			name: "k3s marker label",
			nodes: []NodeInfo{{
				Name:   "n1",
				Labels: map[string]string{"rke2.io/machine": "m1"},
			}},
			want: Flavor{Hints: []string{"rke2"}},
		},
		{
			// k3d is an environment: Name stays the distribution.
			name:  "k3d node name",
			nodes: []NodeInfo{{Name: "k3d-server-0"}},
			want:  Flavor{Name: "k3s", Environment: "k3d", Hints: []string{"k3d"}},
		},
		{
			name:  "k3d via providerID keeps k3s name",
			v:     VersionInfo{Server: ServerVersionInfo{GitVersion: "v1.35.5+k3s1"}},
			nodes: []NodeInfo{{Name: "server", ProviderID: "k3s://k3d-server-0"}},
			want: Flavor{
				VersionSuffix: "k3s1", Name: "k3s",
				ProviderID:  "k3s://k3d-server-0",
				Environment: "k3d", Hints: []string{"k3d"},
			},
		},
		{
			name:  "k3d via rawNodeArgs",
			nodes: []NodeInfo{{Name: "server", rawNodeArgs: "docker inspect k3d-server"}},
			want:  Flavor{Name: "k3s", Environment: "k3d", Hints: []string{"k3d"}},
		},
		{
			// Only the first node feeds flavor detection.
			name: "second node ignored",
			nodes: []NodeInfo{
				{Name: "plain", ProviderID: ""},
				{Name: "k3d-other", ProviderID: "gce://x"},
			},
			want: Flavor{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectFlavor(tc.v, tc.nodes)
			if got.Name != tc.want.Name || got.VersionSuffix != tc.want.VersionSuffix ||
				got.ProviderID != tc.want.ProviderID || got.Environment != tc.want.Environment {
				t.Errorf("detectFlavor = %+v, want %+v", got, tc.want)
			}
			if !equalStrings(got.Hints, tc.want.Hints) {
				t.Errorf("Hints = %v, want %v", got.Hints, tc.want.Hints)
			}
		})
	}
}

func TestFlavorName(t *testing.T) {
	cases := []struct {
		suffix, providerID, want string
	}{
		{suffix: "k3s1", want: "k3s"},
		{suffix: "rke2.1", want: "rke2"},
		{providerID: "kind://kind-control-plane", want: "kind"},
		{providerID: "minikube:///vm", want: "minikube"},
		{providerID: "gke://p/z/n", want: "gke"},
		{providerID: "eks:///node", want: "eks"},
		{providerID: "aks://node", want: "aks"},
		{providerID: "k0s://node", want: "k0s"},
		{providerID: "gce://p/z/n", want: "gce"},
		{suffix: "mydist", want: "mydist"},
		// k3d must never appear as a distribution name.
		{suffix: "k3s1", providerID: "k3s://k3d-1", want: "k3s"},
		{want: ""},
	}
	for _, tc := range cases {
		if got := flavorName(tc.suffix, tc.providerID); got != tc.want {
			t.Errorf("flavorName(%q, %q) = %q, want %q", tc.suffix, tc.providerID, got, tc.want)
		}
	}
}

func TestLooksLikeK3d(t *testing.T) {
	cases := []struct {
		name string
		n    NodeInfo
		want bool
	}{
		{name: "name prefix", n: NodeInfo{Name: "k3d-server-0"}, want: true},
		{name: "providerID", n: NodeInfo{ProviderID: "k3s://k3d-agent"}, want: true},
		{name: "rawNodeArgs", n: NodeInfo{rawNodeArgs: "--token abc k3d-lb"}, want: true},
		{name: "plain", n: NodeInfo{Name: "node-1", ProviderID: "aws:///i-1"}},
	}
	for _, tc := range cases {
		if got := looksLikeK3d(&tc.n); got != tc.want {
			t.Errorf("%s: looksLikeK3d = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
