package probe

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestIsTerminalPhase(t *testing.T) {
	if !isTerminalPhase(corev1.PodSucceeded) || !isTerminalPhase(corev1.PodFailed) {
		t.Error("Succeeded/Failed must be terminal")
	}
	for _, ph := range []corev1.PodPhase{corev1.PodRunning, corev1.PodPending, corev1.PodUnknown, ""} {
		if isTerminalPhase(ph) {
			t.Errorf("%q must not be terminal", ph)
		}
	}
}

func TestFilterCapacity(t *testing.T) {
	if got := filterCapacity(nil); got != nil {
		t.Errorf("nil input = %v, want nil", got)
	}
	in := corev1.ResourceList{
		corev1.ResourceCPU:                    resource.MustParse("4"),
		corev1.ResourceMemory:                 resource.MustParse("8Gi"),
		corev1.ResourcePods:                   resource.MustParse("110"),
		corev1.ResourceName("hugepages-2Mi"):  resource.MustParse("1Gi"),
		corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
	}
	out := filterCapacity(in)
	if _, ok := out["hugepages-2Mi"]; ok {
		t.Error("hugepages-2Mi survived filter")
	}
	for _, k := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourcePods, "nvidia.com/gpu"} {
		if _, ok := out[k]; !ok {
			t.Errorf("%s dropped by filter", k)
		}
	}
	// Deep copy: mutating the source must not change the result.
	in[corev1.ResourceCPU] = resource.MustParse("9")
	if got := out[corev1.ResourceCPU]; got.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("result aliases input: cpu = %s", got.String())
	}
}

func TestAddResourceList(t *testing.T) {
	var nilDst corev1.ResourceList
	addResourceList(nilDst, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")})
	dst := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
	addResourceList(dst, nil)
	addResourceList(dst, corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("200m"),
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	})
	if got := dst[corev1.ResourceCPU]; got.Cmp(resource.MustParse("300m")) != 0 {
		t.Errorf("cpu = %s, want 300m", got.String())
	}
	if _, ok := dst[corev1.ResourceMemory]; !ok {
		t.Error("memory key missing")
	}
}

func TestSubResourceList(t *testing.T) {
	a := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	b := corev1.ResourceList{
		corev1.ResourceCPU:  resource.MustParse("1"),
		corev1.ResourcePods: resource.MustParse("110"),
	}
	got := subResourceList(a, b)
	if cpu := got[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("3")) != 0 {
		t.Errorf("cpu = %s, want 3", cpu.String())
	}
	if mem := got[corev1.ResourceMemory]; mem.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("memory = %s, want 8Gi", mem.String())
	}
	// Keys only present in b are dropped: a defines the result's key set.
	if _, ok := got[corev1.ResourcePods]; ok {
		t.Error("pods (b-only key) survived subtraction")
	}
}

// TestSubHeadroom: the pods slot is allocatable minus the pods that actually
// want to run, because PodRequests never yields a "pods" key.
func TestSubHeadroom(t *testing.T) {
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:  resource.MustParse("4"),
		corev1.ResourcePods: resource.MustParse("110"),
	}
	req := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	got := subHeadroom(alloc, req, 3)
	if cpu := got[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("3")) != 0 {
		t.Errorf("cpu headroom = %s, want 3", cpu.String())
	}
	if pods := got[corev1.ResourcePods]; pods.Cmp(resource.MustParse("107")) != 0 {
		t.Errorf("pods headroom = %s, want 107", pods.String())
	}
}

func mkPod(name, node string, phase corev1.PodPhase, cpu string) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "c", Image: "app:v1"}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if cpu != "" {
		p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		}
	}
	return p
}

func TestActivePodRequests(t *testing.T) {
	pods := []corev1.Pod{
		mkPod("p1", "n1", corev1.PodRunning, "100m"),
		mkPod("p2", "n1", corev1.PodPending, "200m"),
		mkPod("p3", "n1", corev1.PodFailed, "500m"), // terminal: excluded
		mkPod("p4", "", corev1.PodPending, "500m"),  // unscheduled: cluster only
	}
	cluster, perNode, perNodeActive, active := activePodRequests(pods)
	if active != 3 {
		t.Errorf("active = %d, want 3", active)
	}
	if cpu := cluster[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("800m")) != 0 {
		t.Errorf("cluster cpu = %s, want 800m", cpu.String())
	}
	if cpu := perNode["n1"][corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("300m")) != 0 {
		t.Errorf("n1 cpu = %s, want 300m", cpu.String())
	}
	if perNodeActive["n1"] != 2 {
		t.Errorf("perNodeActive[n1] = %d, want 2", perNodeActive["n1"])
	}
	if _, ok := perNode[""]; ok {
		t.Error("unscheduled pod leaked into per-node requests")
	}
}

func mkNode(name, cpu, pods string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:  resource.MustParse(cpu),
				corev1.ResourcePods: resource.MustParse(pods),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:  resource.MustParse(cpu),
				corev1.ResourcePods: resource.MustParse(pods),
			},
		},
	}
}

func TestProbeNodesSuccess(t *testing.T) {
	client := fake.NewClientset()
	for _, n := range []*corev1.Node{mkNode("n2", "2", "110"), mkNode("n1", "4", "110")} {
		if _, err := client.CoreV1().Nodes().Create(context.Background(), n, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed node: %v", err)
		}
	}
	for _, p := range []corev1.Pod{
		mkPod("p1", "n1", corev1.PodRunning, "100m"),
		mkPod("p2", "n1", corev1.PodPending, "200m"),
		mkPod("p3", "n1", corev1.PodSucceeded, ""),
		mkPod("p4", "", corev1.PodPending, "500m"),
	} {
		p := p
		if _, err := client.CoreV1().Pods("default").Create(context.Background(), &p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod: %v", err)
		}
	}
	p := &prober{in: Input{Client: client}}
	sec, errs := p.probeNodes(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if sec.NodeCount != 2 {
		t.Errorf("NodeCount = %d, want 2", sec.NodeCount)
	}
	if len(sec.Nodes) != 2 || sec.Nodes[0].Name != "n1" || sec.Nodes[1].Name != "n2" {
		t.Fatalf("nodes not sorted: %+v", sec.Nodes)
	}
	if sec.Capacity.ActivePods != 3 {
		t.Errorf("ActivePods = %d, want 3", sec.Capacity.ActivePods)
	}
	if cpu := sec.Capacity.Requested[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("800m")) != 0 {
		t.Errorf("cluster requested cpu = %s, want 800m", cpu.String())
	}
	if cpu := sec.Capacity.Allocatable[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("6")) != 0 {
		t.Errorf("allocatable cpu = %s, want 6", cpu.String())
	}
	if pods := sec.Capacity.SchedulingHeadroom[corev1.ResourcePods]; pods.Cmp(resource.MustParse("217")) != 0 {
		t.Errorf("headroom pods = %s, want 217", pods.String())
	}
	n1 := sec.Nodes[0]
	if n1.ActivePodCount != 2 || n1.PodCount != 3 {
		t.Errorf("n1 pod counts: active=%d total=%d, want 2/3", n1.ActivePodCount, n1.PodCount)
	}
	wantPhases := map[string]int{"Running": 1, "Pending": 1, "Succeeded": 1}
	for k, v := range wantPhases {
		if n1.PodsByPhase[k] != v {
			t.Errorf("n1 PodsByPhase[%q] = %d, want %d", k, n1.PodsByPhase[k], v)
		}
	}
	if cpu := n1.Requested[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("300m")) != 0 {
		t.Errorf("n1 requested cpu = %s, want 300m", cpu.String())
	}
	if pods := n1.SchedulingHeadroom[corev1.ResourcePods]; pods.Cmp(resource.MustParse("108")) != 0 {
		t.Errorf("n1 headroom pods = %s, want 108", pods.String())
	}
}

func TestProbeNodesForbidden(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
	client := fake.NewClientset()
	client.PrependReactor("list", "nodes",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})
	p := &prober{in: Input{Client: client}}
	sec, errs := p.probeNodes(context.Background())
	if len(errs) != 1 || errs[0].Code != "Forbidden" ||
		errs[0].Section != "nodes" || errs[0].Resource != "nodes" {
		t.Fatalf("errs = %+v, want single Forbidden on nodes/nodes", errs)
	}
	if sec.NodeCount != 0 || sec.Nodes != nil {
		t.Errorf("section not zero: %+v", sec)
	}
}

// TestProbeNodesPodsForbidden: nodes still report when the pod list fails;
// requests and headroom stay nil instead of pretending the cluster is empty.
func TestProbeNodesPodsForbidden(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
	client := fake.NewClientset()
	if _, err := client.CoreV1().Nodes().Create(context.Background(), mkNode("n1", "4", "110"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	client.PrependReactor("list", "pods",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})
	p := &prober{in: Input{Client: client}}
	sec, errs := p.probeNodes(context.Background())
	if len(errs) != 1 || errs[0].Resource != "pods" || errs[0].Code != "Forbidden" {
		t.Fatalf("errs = %+v", errs)
	}
	if len(sec.Nodes) != 1 || sec.Nodes[0].Name != "n1" {
		t.Fatalf("nodes = %+v", sec.Nodes)
	}
	if sec.Nodes[0].Requested != nil || sec.Nodes[0].SchedulingHeadroom != nil {
		t.Error("requests must stay nil when pod data is unknown")
	}
	if sec.Capacity.Requested != nil || sec.Capacity.SchedulingHeadroom != nil {
		t.Error("cluster capacity requests must stay nil when pod data is unknown")
	}
	if st := classify(sec, errs); st.Status != StatusPartial {
		t.Errorf("classify = %+v, want partial", st)
	}
}

// TestListAllPodsPagination: the fake API ignores Limit/Continue, so pages
// are served by a reactor keyed on call count.
func TestListAllPodsPagination(t *testing.T) {
	client := fake.NewClientset()
	calls := 0
	client.PrependReactor("list", "pods",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls == 1 {
				return true, &corev1.PodList{
					Items:    []corev1.Pod{mkPod("a", "", corev1.PodRunning, ""), mkPod("b", "", corev1.PodRunning, "")},
					ListMeta: metav1.ListMeta{Continue: "tok"},
				}, nil
			}
			return true, &corev1.PodList{Items: []corev1.Pod{mkPod("c", "", corev1.PodRunning, "")}}, nil
		})
	p := &prober{in: Input{Client: client}}
	pods, errs := p.listAllPods(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if calls != 2 {
		t.Errorf("list calls = %d, want 2", calls)
	}
	if len(pods) != 3 || pods[0].Name != "a" || pods[2].Name != "c" {
		t.Errorf("pods = %d items, want 3 pages stitched", len(pods))
	}
}

// TestListAllPodsPageError: a failure on page 2 keeps page 1's pods and
// reports the error.
func TestListAllPodsPageError(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
	client := fake.NewClientset()
	calls := 0
	client.PrependReactor("list", "pods",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls == 1 {
				return true, &corev1.PodList{
					Items:    []corev1.Pod{mkPod("a", "", corev1.PodRunning, "")},
					ListMeta: metav1.ListMeta{Continue: "tok"},
				}, nil
			}
			return true, nil, forbidden
		})
	p := &prober{in: Input{Client: client}}
	pods, errs := p.listAllPods(context.Background())
	if len(pods) != 1 || pods[0].Name != "a" {
		t.Errorf("pods = %v, want page 1 kept", pods)
	}
	if len(errs) != 1 || errs[0].Code != "Forbidden" || errs[0].Resource != "pods" {
		t.Errorf("errs = %+v, want Forbidden on pods", errs)
	}
}

func TestNodeInfo(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "n1",
			CreationTimestamp: metav1.NewTime(t0),
			Labels: map[string]string{
				"kubernetes.io/hostname": "n1",
				"custom.io/drop":         "x",
			},
			Annotations: map[string]string{
				"k3s.io/node-args": "server --token abc",
				"k3s.io/hostname":  "n1",
				"custom.io/drop":   "y",
			},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///i-1",
			PodCIDR:    "10.244.0.0/24",
			Taints: []corev1.Taint{
				{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule},
			},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:                   resource.MustParse("4"),
				corev1.ResourcePods:                  resource.MustParse("110"),
				corev1.ResourceName("hugepages-2Mi"): resource.MustParse("1Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:  resource.MustParse("4"),
				corev1.ResourcePods: resource.MustParse("110"),
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
			},
			Conditions: []corev1.NodeCondition{
				{
					Type: corev1.NodeReady, Status: corev1.ConditionTrue,
					Reason: "KubeletReady", Message: "ready",
					LastHeartbeatTime: metav1.NewTime(t0.Add(time.Minute)),
				},
			},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion: "v1.31.0", OSImage: "Ubuntu 22.04",
				KernelVersion: "6.1.0", ContainerRuntimeVersion: "containerd://1.7.0",
				Architecture: "amd64", OperatingSystem: "linux",
			},
		},
	}
	requested := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
	info := nodeInfo(n, requested, 1, map[string]int{"Running": 2})

	if info.Name != "n1" || info.ProviderID != "aws:///i-1" || info.PodCIDR != "10.244.0.0/24" {
		t.Errorf("identity fields wrong: %+v", info)
	}
	if info.KubeletVersion != "v1.31.0" || info.OSImage != "Ubuntu 22.04" ||
		info.ContainerRuntimeVersion != "containerd://1.7.0" || info.OS != "linux" {
		t.Errorf("system info wrong: %+v", info)
	}
	if _, ok := info.Capacity["hugepages-2Mi"]; ok {
		t.Error("hugepages leaked into node capacity")
	}
	if len(info.Labels) != 1 || info.Labels["kubernetes.io/hostname"] != "n1" {
		t.Errorf("labels = %v, want only allowlisted", info.Labels)
	}
	if len(info.Annotations) != 1 || info.Annotations["k3s.io/hostname"] != "n1" {
		t.Errorf("annotations = %v, want only allowlisted", info.Annotations)
	}
	if info.rawNodeArgs != "server --token abc" {
		t.Errorf("rawNodeArgs = %q", info.rawNodeArgs)
	}
	if info.ActivePodCount != 1 || info.PodCount != 2 {
		t.Errorf("pod counts: active=%d total=%d, want 1/2", info.ActivePodCount, info.PodCount)
	}
	if cpu := info.Requested[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("requested cpu = %s", cpu.String())
	}
	if pods := info.SchedulingHeadroom[corev1.ResourcePods]; pods.Cmp(resource.MustParse("109")) != 0 {
		t.Errorf("headroom pods = %s, want 109", pods.String())
	}
	if len(info.Conditions) != 1 || info.Conditions[0].Type != "Ready" ||
		info.Conditions[0].Status != "True" || info.Conditions[0].Message != "ready" {
		t.Errorf("conditions = %+v", info.Conditions)
	}
	if len(info.Taints) != 1 || info.Taints[0].Key != "node.kubernetes.io/unschedulable" {
		t.Errorf("taints = %+v", info.Taints)
	}
	if len(info.Addresses) != 1 || info.Addresses[0].Address != "10.0.0.5" {
		t.Errorf("addresses = %+v", info.Addresses)
	}
	if !info.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v, want %v", info.CreatedAt, t0)
	}

	// requested == nil (pod list failed): unknown, not zero.
	unknown := nodeInfo(n, nil, 0, nil)
	if unknown.Requested != nil || unknown.SchedulingHeadroom != nil {
		t.Error("nil requested must keep requests/headroom unset")
	}
}
