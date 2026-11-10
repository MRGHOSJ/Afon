package probe

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// seedNamespace creates the target namespace with allowlisted and custom
// labels/annotations; returns its creation timestamp.
func seedNamespace(t *testing.T, client *fake.Clientset) time.Time {
	t.Helper()
	created := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "team-a",
			UID:               "ns-uid-1",
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				"kubernetes.io/metadata.name":        "team-a",
				"pod-security.kubernetes.io/enforce": "restricted",
				"custom.io/drop":                     "x",
			},
			Annotations: map[string]string{"custom.io/note": "n"},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
	if _, err := client.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	return created
}

func TestProbeNamespaceSuccess(t *testing.T) {
	client := fake.NewClientset()
	created := seedNamespace(t, client)
	if _, err := client.CoreV1().ResourceQuotas("team-a").Create(context.Background(), &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-quota", Namespace: "team-a"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10")},
			Used: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5")},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().LimitRanges("team-a").Create(context.Background(), &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-limits", Namespace: "team-a"},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{
			{
				Type:           corev1.LimitTypeContainer,
				Max:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				Min:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
				Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeNamespace(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if !info.Exists || info.Name != "team-a" || info.UID != "ns-uid-1" || info.Phase != "Active" {
		t.Errorf("identity fields = %+v", info)
	}
	if info.CreatedAt == nil || !info.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", info.CreatedAt, created)
	}
	if len(info.Labels) != 2 ||
		info.Labels["kubernetes.io/metadata.name"] != "team-a" ||
		info.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("Labels = %v, want only the two allowlisted keys", info.Labels)
	}
	if info.Annotations != nil {
		t.Errorf("Annotations = %v, want nil (all dropped)", info.Annotations)
	}
	if len(info.ResourceQuotas) != 1 {
		t.Fatalf("quotas = %+v", info.ResourceQuotas)
	}
	q := info.ResourceQuotas[0]
	if q.Name != "team-a-quota" ||
		q.Hard.Cpu().Cmp(resource.MustParse("10")) != 0 ||
		q.Used.Cpu().Cmp(resource.MustParse("5")) != 0 {
		t.Errorf("quota = %+v", q)
	}
	if len(info.LimitRanges) != 1 || info.LimitRanges[0].Name != "team-a-limits" {
		t.Fatalf("limit ranges = %+v", info.LimitRanges)
	}
	item := info.LimitRanges[0].Items[0]
	if item.Type != "Container" ||
		item.Max.Cpu().Cmp(resource.MustParse("2")) != 0 ||
		item.Min.Cpu().Cmp(resource.MustParse("10m")) != 0 {
		t.Errorf("limit item = %+v", item)
	}
}

// TestProbeNamespaceMissing: a namespace that does not exist is a fact
// (Exists false, Name kept), not an error.
func TestProbeNamespaceMissing(t *testing.T) {
	client := fake.NewClientset()
	p := &prober{in: Input{Client: client, Namespace: "ghost"}}
	info, errs := p.probeNamespace(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if info.Exists || info.Name != "ghost" {
		t.Errorf("info = %+v, want {Name: ghost}", info)
	}
}

func TestProbeNamespaceGetForbidden(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("get", "namespaces",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeNamespace(context.Background())
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want 1", errs)
	}
	got := errs[0]
	if got.Section != "namespace" || got.Resource != "namespaces/team-a" || got.Code != "Forbidden" {
		t.Errorf("ProbeError = %+v", got)
	}
	if !isZero(info) {
		t.Errorf("info = %+v, want zero value", info)
	}
}

// TestProbeNamespaceQuotaForbidden: the quota list failing costs only the
// quotas; the namespace facts and limit ranges survive.
func TestProbeNamespaceQuotaForbidden(t *testing.T) {
	client := fake.NewClientset()
	seedNamespace(t, client)
	if _, err := client.CoreV1().LimitRanges("team-a").Create(context.Background(), &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-limits", Namespace: "team-a"},
		Spec:       corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "resourcequotas",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeNamespace(context.Background())
	if len(errs) != 1 || errs[0].Resource != "resourcequotas" || errs[0].Code != "Forbidden" {
		t.Fatalf("errs = %+v", errs)
	}
	if !info.Exists || info.ResourceQuotas != nil {
		t.Errorf("quotas should be nil, info = %+v", info)
	}
	if len(info.LimitRanges) != 1 {
		t.Errorf("limit ranges lost: %+v", info.LimitRanges)
	}
	if st := classify(info, errs); st.Status != StatusPartial {
		t.Errorf("classify = %+v, want partial", st)
	}
}

// TestProbeNamespaceLimitRangeForbidden: mirror of the quota case; errs keep
// the assembly order quotas first, then limit ranges.
func TestProbeNamespaceLimitRangeForbidden(t *testing.T) {
	client := fake.NewClientset()
	seedNamespace(t, client)
	client.PrependReactor("list", "limitranges",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	client.PrependReactor("list", "resourcequotas",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeNamespace(context.Background())
	if len(errs) != 2 {
		t.Fatalf("errs = %+v, want 2", errs)
	}
	if errs[0].Resource != "resourcequotas" || errs[1].Resource != "limitranges" {
		t.Errorf("err order = %+v", errs)
	}
	if !info.Exists {
		t.Error("namespace facts lost")
	}
}
