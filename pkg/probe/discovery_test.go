package probe

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	dfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// errForbidden is a shared stand-in for a permissions failure.
func errForbidden() error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "x"}, "", nil)
}

func seedDiscovery(client *fake.Clientset) *dfake.FakeDiscovery {
	fd := client.Discovery().(*dfake.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"list", "watch"}},
				{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
				{Name: "services", Kind: "Service", Namespaced: true, Verbs: []string{"create"}},
			},
		},
		{
			GroupVersion: "metrics.k8s.io/v1beta1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Kind: "PodMetrics", Namespaced: true, Verbs: []string{"list"}},
			},
		},
	}
	return fd
}

func TestProbeDiscoverySuccess(t *testing.T) {
	client := fake.NewClientset()
	seedDiscovery(client)
	p := &prober{in: Input{Client: client}}
	info, errs := p.probeDiscovery(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if info.GroupCount != 2 || info.ResourceCount != 4 {
		t.Errorf("counts = %d groups / %d resources, want 2/4", info.GroupCount, info.ResourceCount)
	}
	if len(info.Groups) != 2 || info.Groups[0].Name != "" || info.Groups[1].Name != "metrics.k8s.io" {
		t.Fatalf("groups not sorted as expected: %+v", info.Groups)
	}
	core := info.Groups[0]
	if core.PreferredVersion != "v1" || core.ResourceCount != 3 ||
		len(core.Versions) != 1 || core.Versions[0] != "v1" {
		t.Errorf("core summary = %+v", core)
	}
	if m := info.Groups[1]; m.PreferredVersion != "v1beta1" || m.ResourceCount != 1 {
		t.Errorf("metrics summary = %+v", m)
	}
	// Every capability key is present, only metrics.k8s.io is on.
	if len(info.Capabilities) != len(capabilityGroups) {
		t.Errorf("capabilities = %d keys, want %d", len(info.Capabilities), len(capabilityGroups))
	}
	if !info.Capabilities["metricsServer"] {
		t.Error("metricsServer capability = false, want true")
	}
	for _, cap := range capabilityGroups {
		if cap.Name == "metricsServer" {
			continue
		}
		if info.Capabilities[cap.Name] {
			t.Errorf("capability %s = true, want false", cap.Name)
		}
	}
	if info.Full != nil {
		t.Errorf("Full populated without Input.Full: %+v", info.Full)
	}
	if st := classify(info, errs); st.Status != StatusOK {
		t.Errorf("classify = %+v, want ok", st)
	}
}

func TestProbeDiscoveryFull(t *testing.T) {
	client := fake.NewClientset()
	seedDiscovery(client)
	p := &prober{in: Input{Client: client, Full: true}}
	info, errs := p.probeDiscovery(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(info.Full) != 2 {
		t.Fatalf("Full = %d groups, want 2", len(info.Full))
	}
	// Core group first, subresources excluded, resources sorted.
	core := info.Full[0]
	if core.Name != "" || core.ResourceCount != 2 {
		t.Fatalf("core detail = %+v, want 2 resources (no pods/log)", core)
	}
	names := []string{core.Resources[0].Name, core.Resources[1].Name}
	if names[0] != "pods" || names[1] != "services" {
		t.Errorf("resources = %v, want [pods services]", names)
	}
	if core.Resources[0].Kind != "Pod" || !core.Resources[0].Namespaced ||
		len(core.Resources[0].Verbs) != 2 {
		t.Errorf("resource detail = %+v", core.Resources[0])
	}
	if m := info.Full[1]; m.Name != "metrics.k8s.io" || m.ResourceCount != 1 {
		t.Errorf("metrics detail = %+v", m)
	}
}

// TestProbeDiscoveryPartial: one broken aggregated API yields one structured
// error while the rest of the API surface is kept.
func TestProbeDiscoveryPartial(t *testing.T) {
	client := fake.NewClientset()
	seedDiscovery(client)
	broken := &discovery.ErrGroupDiscoveryFailed{
		Groups: map[schema.GroupVersion]error{
			{Group: "custom.io", Version: "v1"}: errors.New("broken APIService"),
		},
	}
	client.PrependReactor("get", "resource",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, broken
		})
	p := &prober{in: Input{Client: client}}
	info, errs := p.probeDiscovery(context.Background())
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want 1", errs)
	}
	got := errs[0]
	if got.Section != "discovery" || got.Resource != "custom.io/v1" ||
		got.Reason != "broken APIService" || got.Code != "PartialDiscovery" {
		t.Errorf("ProbeError = %+v", got)
	}
	if info.GroupCount != 2 || info.ResourceCount != 4 {
		t.Errorf("usable data lost: %+v", info)
	}
	if st := classify(info, errs); st.Status != StatusPartial {
		t.Errorf("classify = %+v, want partial", st)
	}
}

// TestProbeDiscoveryGroupListForbidden: without any groups at all the probe
// returns a zero section plus the structured error.
func TestProbeDiscoveryGroupListForbidden(t *testing.T) {
	client := fake.NewClientset()
	seedDiscovery(client)
	client.PrependReactor("get", "group",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	p := &prober{in: Input{Client: client}}
	info, errs := p.probeDiscovery(context.Background())
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want 1", errs)
	}
	got := errs[0]
	if got.Section != "discovery" || got.Resource != "api-resources" ||
		got.Code != "Forbidden" {
		t.Errorf("ProbeError = %+v", got)
	}
	if !isZero(info) {
		t.Errorf("info = %+v, want zero value", info)
	}
}

func TestGroupDetailsDedupAndSubresources(t *testing.T) {
	groups := []*metav1.APIGroup{
		{
			Name:             "apps",
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"},
			Versions: []metav1.GroupVersionForDiscovery{
				{GroupVersion: "apps/v1", Version: "v1"},
				{GroupVersion: "apps/v1beta1", Version: "v1beta1"},
			},
		},
	}
	resources := []*metav1.APIResourceList{
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "statefulsets", Kind: "StatefulSet", Namespaced: true},
				{Name: "deployments", Kind: "Deployment", Namespaced: true},
				{Name: "deployments/scale", Kind: "Scale", Namespaced: true},
			},
		},
		{
			GroupVersion: "apps/v1beta1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Kind: "Deployment", Namespaced: true},
			},
		},
	}
	details := groupDetails(groups, resources)
	if len(details) != 1 {
		t.Fatalf("details = %d, want 1", len(details))
	}
	d := details[0]
	if d.Name != "apps" || d.PreferredVersion != "v1" || len(d.Versions) != 2 {
		t.Errorf("detail header = %+v", d)
	}
	// deployments appears once despite two versions; the scale subresource
	// is dropped; names come out sorted.
	if d.ResourceCount != 2 {
		t.Errorf("ResourceCount = %d, want 2", d.ResourceCount)
	}
	if d.Resources[0].Name != "deployments" || d.Resources[1].Name != "statefulsets" {
		t.Errorf("resources = %+v, want [deployments statefulsets]", d.Resources)
	}
}
