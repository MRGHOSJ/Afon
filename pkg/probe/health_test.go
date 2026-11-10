package probe

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestReadyzSummary(t *testing.T) {
	verbose := "[+] ok\n[+] livez\n[-] etcd [6.5] something broke\n  detail line\n[+] shutdown"
	cases := []struct {
		name string
		body string
		full bool
		want string
	}{
		{name: "failed checks only", body: verbose, want: "[-] etcd [6.5] something broke"},
		{name: "full keeps everything", body: verbose, full: true, want: verbose},
		{name: "all passing is empty", body: "[+] a\n[+] b", want: ""},
		{name: "no check lines kept verbatim", body: "  etcd is unhealthy  ", want: "etcd is unhealthy"},
		{name: "empty body", body: "", want: ""},
		{name: "multiple failures joined", body: "[-] a\n[+] ok\n[-] b", want: "[-] a\n[-] b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readyzSummary(tc.body, tc.full); got != tc.want {
				t.Errorf("readyzSummary(%q, %v) = %q, want %q", tc.body, tc.full, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("12345", 5); got != "12345" {
		t.Errorf("exact length = %q, want unchanged", got)
	}
	if got := truncate("123456", 5); got != "12345..." {
		t.Errorf("over length = %q, want 12345...", got)
	}
	if got := truncate("", 5); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
	long := strings.Repeat("x", 3000)
	if got := truncate(long, 2048); len(got) != 2051 || !strings.HasSuffix(got, "...") {
		t.Errorf("long truncate length = %d, want 2051 with ellipsis", len(got))
	}
}

func TestSortComponents(t *testing.T) {
	cs := []ComponentInfo{
		{Name: "z", Kind: "Deployment"},
		{Name: "b", Kind: "DaemonSet"},
		{Name: "a", Kind: "Deployment"},
		{Name: "a", Kind: "DaemonSet"},
	}
	sortComponents(cs)
	want := []ComponentInfo{
		{Name: "a", Kind: "DaemonSet"},
		{Name: "b", Kind: "DaemonSet"},
		{Name: "a", Kind: "Deployment"},
		{Name: "z", Kind: "Deployment"},
	}
	for i := range want {
		if cs[i].Name != want[i].Name || cs[i].Kind != want[i].Kind {
			t.Errorf("cs[%d] = %+v, want %+v", i, cs[i], want[i])
		}
	}
}

func mkDeploy(name string, replicas int32, ready int32, image string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
	if replicas >= 0 {
		d.Spec.Replicas = &replicas
	}
	return d
}

func mkDaemonSet(name string, desired, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "kube-proxy:v1.31"}}},
			},
		},
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: desired, NumberReady: ready},
	}
}

// TestProbeHealth covers the fake-client path: the discovery REST client is
// nil (recorded as Unsupported) while deployments and daemonsets populate the
// component list in sorted order, with CoreDNS lifted out.
func TestProbeHealth(t *testing.T) {
	client := fake.NewClientset()
	apps := client.AppsV1()
	if _, err := apps.Deployments("kube-system").Create(context.Background(),
		mkDeploy("coredns", 2, 2, "coredns/coredns:v1.11"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Deployments("kube-system").Create(context.Background(),
		mkDeploy("zzz", -1, 0, "zzz:v1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.DaemonSets("kube-system").Create(context.Background(),
		mkDaemonSet("kube-proxy", 3, 3), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	p := &prober{in: Input{Client: client}}
	h, errs := p.probeHealth(context.Background())

	if len(errs) != 1 || errs[0].Resource != "readyz" || errs[0].Code != "Unsupported" {
		t.Fatalf("errs = %+v, want readyz Unsupported", errs)
	}
	if h.Ready {
		t.Error("Ready = true, fake has no /readyz")
	}
	if h.Readyz != "" {
		t.Errorf("Readyz = %q, want empty", h.Readyz)
	}
	if h.CoreDNS.Desired != 2 || h.CoreDNS.Ready != 2 {
		t.Errorf("CoreDNS = %+v, want 2/2", h.CoreDNS)
	}
	if len(h.Components) != 3 {
		t.Fatalf("components = %d, want 3", len(h.Components))
	}
	wantOrder := []struct {
		name, kind string
	}{
		{"kube-proxy", "DaemonSet"},
		{"coredns", "Deployment"},
		{"zzz", "Deployment"},
	}
	for i, w := range wantOrder {
		if h.Components[i].Name != w.name || h.Components[i].Kind != w.kind {
			t.Errorf("Components[%d] = %+v, want %s %s", i, h.Components[i], w.kind, w.name)
		}
	}
	// Replicas nil means desired defaults to 1.
	if h.Components[2].Desired != 1 {
		t.Errorf("zzz Desired = %d, want 1 (nil replicas default)", h.Components[2].Desired)
	}
	if len(h.Components[0].Images) != 1 || h.Components[0].Images[0] != "kube-proxy:v1.31" {
		t.Errorf("kube-proxy images = %v", h.Components[0].Images)
	}
	if st := classify(h, errs); st.Status != StatusPartial {
		t.Errorf("classify = %+v, want partial", st)
	}
}

// TestProbeHealthDeploymentsForbidden: denied deployment list costs the
// deployments (including CoreDNS) but daemonsets still land.
func TestProbeHealthDeploymentsForbidden(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "", nil)
	client := fake.NewClientset()
	client.PrependReactor("list", "deployments",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})
	if _, err := client.AppsV1().DaemonSets("kube-system").Create(context.Background(),
		mkDaemonSet("kube-proxy", 3, 3), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := &prober{in: Input{Client: client}}
	h, errs := p.probeHealth(context.Background())

	if len(errs) != 2 {
		t.Fatalf("errs = %+v, want readyz + deployments", errs)
	}
	if errs[1].Section != "health" || errs[1].Resource != "deployments/kube-system" ||
		errs[1].Code != "Forbidden" {
		t.Errorf("deployments err = %+v", errs[1])
	}
	if len(h.Components) != 1 || h.Components[0].Name != "kube-proxy" {
		t.Errorf("components = %+v, want only daemonset", h.Components)
	}
	if h.CoreDNS != (CoreDNSInfo{}) {
		t.Errorf("CoreDNS = %+v, want zero without deployments", h.CoreDNS)
	}
}

func TestContainerImages(t *testing.T) {
	got := containerImages([]corev1.Container{
		{Image: "a:v1"}, {Image: ""}, {Image: "a:v1"}, {Image: "b:v2"},
	})
	if len(got) != 2 || got[0] != "a:v1" || got[1] != "b:v2" {
		t.Errorf("images = %v, want [a:v1 b:v2]", got)
	}
	if imgs := containerImages(nil); imgs != nil {
		t.Errorf("nil containers = %v, want nil", imgs)
	}
}
