package probe

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-helpers/resource"
)

type ConditionInfo struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastHeartbeatTime  time.Time `json:"lastHeartbeatTime,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime,omitempty"`
}

type TaintInfo struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

type AddressInfo struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

type NodeInfo struct {
	Name                    string              `json:"name"`
	ProviderID              string              `json:"providerID,omitempty"`
	KubeletVersion          string              `json:"kubeletVersion"`
	OSImage                 string              `json:"osImage,omitempty"`
	KernelVersion           string              `json:"kernelVersion,omitempty"`
	ContainerRuntimeVersion string              `json:"containerRuntimeVersion,omitempty"`
	Architecture            string              `json:"architecture,omitempty"`
	OS                      string              `json:"os,omitempty"`
	Capacity                corev1.ResourceList `json:"capacity,omitempty"`
	Allocatable             corev1.ResourceList `json:"allocatable,omitempty"`
	Requested               corev1.ResourceList `json:"requested,omitempty"`
	SchedulingHeadroom      corev1.ResourceList `json:"schedulingHeadroom,omitempty"`
	ActivePodCount          int                 `json:"activePodCount"`
	PodCount                int                 `json:"podCount"`
	PodsByPhase             map[string]int      `json:"podsByPhase,omitempty"`
	Conditions              []ConditionInfo     `json:"conditions,omitempty"`
	Taints                  []TaintInfo         `json:"taints,omitempty"`
	Labels                  map[string]string   `json:"labels,omitempty"`
	Annotations             map[string]string   `json:"annotations,omitempty"`
	Addresses               []AddressInfo       `json:"addresses,omitempty"`
	PodCIDR                 string              `json:"podCIDR,omitempty"`
	PodCIDRs                []string            `json:"podCIDRs,omitempty"`
	CreatedAt               time.Time           `json:"createdAt"`
	// rawNodeArgs keeps k3s.io/node-args for flavor detection: the allowlist
	// drops it from Annotations (server command line, tokens), and unexported
	// fields never serialize.
	rawNodeArgs string
}

// CapacitySummary is cluster-level scheduling capacity. SchedulingHeadroom is
// allocatable minus requested it is scheduling headroom, NOT usage: it says
// how much more can be scheduled, not how much is free in practice. Its pods
// slot is allocatable pods minus active pods (subHeadroom), because pod
// requests never include a pods key.
type CapacitySummary struct {
	Allocatable        corev1.ResourceList `json:"allocatable,omitempty"`
	Requested          corev1.ResourceList `json:"requested,omitempty"`
	SchedulingHeadroom corev1.ResourceList `json:"schedulingHeadroom,omitempty"`
	ActivePods         int                 `json:"activePods"`
}

type NodesSection struct {
	NodeCount int             `json:"nodeCount"`
	Nodes     []NodeInfo      `json:"nodes"`
	Capacity  CapacitySummary `json:"capacity"`
}

// podListPage is the page size for cluster-wide pod listing; big clusters are
// walked via the continue token instead of one huge response.
const podListPage = 500

func (p *prober) probeNodes(ctx context.Context) (NodesSection, []ProbeError) {
	var sec NodesSection

	// Nodes list and the (paginated) cluster-wide pod list are independent;
	// fetch them concurrently.
	var (
		wg       sync.WaitGroup
		nodeList *corev1.NodeList
		nodesErr *ProbeError
		pods     []corev1.Pod
		podErrs  []ProbeError
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		var err error
		nodeList, err = p.in.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("nodes", "nodes", err)
			nodesErr = &pe
		}
	}()
	go func() {
		defer wg.Done()
		pods, podErrs = p.listAllPods(ctx)
	}()
	wg.Wait()

	if nodesErr != nil {
		return NodesSection{}, []ProbeError{*nodesErr}
	}
	sec.Nodes = []NodeInfo{}
	errs := podErrs

	perNodePhases := map[string]map[string]int{}
	for i := range pods {
		pod := &pods[i]
		node := pod.Spec.NodeName
		if node == "" {
			continue
		}
		if perNodePhases[node] == nil {
			perNodePhases[node] = map[string]int{}
		}
		perNodePhases[node][string(pod.Status.Phase)]++
	}

	clusterReqs, perNodeReqs, perNodeActive, activePods := activePodRequests(pods)

	sec.NodeCount = len(nodeList.Items)
	sec.Capacity.ActivePods = activePods
	// Cluster requested includes unscheduled pods (they still want capacity
	// somewhere); per-node requested only covers pods bound to that node.
	// If the pod list failed we know nothing about requests leave them nil
	// rather than claiming the whole allocatable as headroom.
	if len(podErrs) == 0 {
		sec.Capacity.Requested = filterCapacity(clusterReqs)
		rawAlloc := corev1.ResourceList{}
		for i := range nodeList.Items {
			n := &nodeList.Items[i]
			reqs := perNodeReqs[n.Name]
			if reqs == nil {
				reqs = corev1.ResourceList{} // pod data known: zero requests, not unknown
			}
			info := nodeInfo(n, reqs, perNodeActive[n.Name], perNodePhases[n.Name])
			sec.Nodes = append(sec.Nodes, info)
			addResourceList(rawAlloc, n.Status.Allocatable)
		}
		sec.Capacity.SchedulingHeadroom = filterCapacity(subHeadroom(rawAlloc, clusterReqs, activePods))
		sec.Capacity.Allocatable = filterCapacity(rawAlloc)
	} else {
		for i := range nodeList.Items {
			n := &nodeList.Items[i]
			sec.Nodes = append(sec.Nodes, nodeInfo(n, nil, 0, perNodePhases[n.Name]))
		}
	}
	sort.Slice(sec.Nodes, func(i, j int) bool { return sec.Nodes[i].Name < sec.Nodes[j].Name })
	return sec, errs
}

// activePodRequests sums effective requests for pods that still hold
// resources. Terminal pods (Succeeded/Failed) are excluded from the sums but
// remain visible in pods-by-phase. The calculation delegates to
// k8s.io/component-helpers/resource.PodRequests so init containers, sidecar
// (restartable init) containers, pod overhead, and pod-level resources are
// handled the way the scheduler does. Cluster totals include unscheduled
// pods; per-node totals only cover pods bound to that node.
func activePodRequests(pods []corev1.Pod) (cluster corev1.ResourceList, perNode map[string]corev1.ResourceList, perNodeActive map[string]int, active int) {
	cluster = corev1.ResourceList{}
	perNode = map[string]corev1.ResourceList{}
	perNodeActive = map[string]int{}
	for i := range pods {
		pod := &pods[i]
		if isTerminalPhase(pod.Status.Phase) {
			continue
		}
		active++
		reqs := resource.PodRequests(pod, resource.PodResourcesOptions{})
		addResourceList(cluster, reqs)
		if node := pod.Spec.NodeName; node != "" {
			perNodeActive[node]++
			if perNode[node] == nil {
				perNode[node] = corev1.ResourceList{}
			}
			addResourceList(perNode[node], reqs)
		}
	}
	return cluster, perNode, perNodeActive, active
}

// listAllPods walks the cluster-wide pod list page by page.
func (p *prober) listAllPods(ctx context.Context) ([]corev1.Pod, []ProbeError) {
	var all []corev1.Pod
	opts := metav1.ListOptions{Limit: podListPage}
	for {
		page, err := p.in.Client.CoreV1().Pods("").List(ctx, opts)
		if err != nil {
			return all, []ProbeError{asProbeError("nodes", "pods", err)}
		}
		all = append(all, page.Items...)
		if page.Continue == "" {
			return all, nil
		}
		opts.Continue = page.Continue
	}
}

func isTerminalPhase(p corev1.PodPhase) bool {
	return p == corev1.PodSucceeded || p == corev1.PodFailed
}

func nodeInfo(n *corev1.Node, requested corev1.ResourceList, activePods int, phases map[string]int) NodeInfo {
	info := NodeInfo{
		Name:                    n.Name,
		ProviderID:              n.Spec.ProviderID,
		KubeletVersion:          n.Status.NodeInfo.KubeletVersion,
		OSImage:                 n.Status.NodeInfo.OSImage,
		KernelVersion:           n.Status.NodeInfo.KernelVersion,
		ContainerRuntimeVersion: n.Status.NodeInfo.ContainerRuntimeVersion,
		Architecture:            n.Status.NodeInfo.Architecture,
		OS:                      n.Status.NodeInfo.OperatingSystem,
		Capacity:                filterCapacity(n.Status.Capacity),
		Allocatable:             filterCapacity(n.Status.Allocatable),
		ActivePodCount:          activePods,
		PodsByPhase:             phases,
		Labels:                  allowMap(n.Labels),
		Annotations:             allowMap(n.Annotations),
		PodCIDR:                 n.Spec.PodCIDR,
		PodCIDRs:                n.Spec.PodCIDRs,
		CreatedAt:               n.CreationTimestamp.Time.UTC(),
		// Captured before allowMap strips k3s.io/node-args (see mapKeyExact);
		// unexported, so it never reaches JSON or Redact.
		rawNodeArgs: n.Annotations["k3s.io/node-args"],
	}
	// requested == nil means the pod list failed: requests and headroom are
	// unknown, so leave them unset rather than inventing numbers.
	if requested != nil {
		info.Requested = filterCapacity(requested)
		info.SchedulingHeadroom = filterCapacity(subHeadroom(n.Status.Allocatable, requested, activePods))
	}
	for _, c := range n.Status.Conditions {
		info.Conditions = append(info.Conditions, ConditionInfo{
			Type:               string(c.Type),
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			LastHeartbeatTime:  c.LastHeartbeatTime.Time.UTC(),
			LastTransitionTime: c.LastTransitionTime.Time.UTC(),
		})
	}
	for _, t := range n.Spec.Taints {
		info.Taints = append(info.Taints, TaintInfo{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
	}
	for _, a := range n.Status.Addresses {
		info.Addresses = append(info.Addresses, AddressInfo{Type: string(a.Type), Address: a.Address})
	}
	// per-node pod count from phases map (all phases, incl. terminal)
	for _, c := range phases {
		info.PodCount += c
	}
	return info
}

func addResourceList(dst, src corev1.ResourceList) {
	if dst == nil || src == nil {
		return
	}
	for name, qty := range src {
		if existing, ok := dst[name]; ok {
			existing.Add(qty)
			dst[name] = existing
		} else {
			dst[name] = qty.DeepCopy()
		}
	}
}

func subResourceList(a, b corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{}
	for name, qty := range a {
		out[name] = qty.DeepCopy()
	}
	for name, qty := range b {
		if existing, ok := out[name]; ok {
			existing.Sub(qty)
			out[name] = existing
		}
	}
	return out
}

// subHeadroom is allocatable minus requested, with the pod-capacity slot
// corrected: PodRequests never produces a "pods" key, so plain subtraction
// would leave headroom.pods claiming the whole allocatable. Headroom.pods is
// capacity minus the pods that actually want to run.
func subHeadroom(allocatable, requested corev1.ResourceList, activePods int) corev1.ResourceList {
	out := subResourceList(allocatable, requested)
	if q, ok := allocatable[corev1.ResourcePods]; ok {
		pods := q.DeepCopy()
		pods.Sub(*apiresource.NewQuantity(int64(activePods), q.Format))
		out[corev1.ResourcePods] = pods
	}
	return out
}

// filterCapacity drops keys that add bytes but no scheduling signal:
// hugepages-*. Everything else survives, including extended resources like
// nvidia.com/gpu. Applied to node/cluster capacity lists only never to
// quota hard/used, whose keys (requests.cpu, count/…, services.loadbalancers)
// are not resource names.
func filterCapacity(rl corev1.ResourceList) corev1.ResourceList {
	if rl == nil {
		return nil
	}
	out := corev1.ResourceList{}
	for k, v := range rl {
		if strings.HasPrefix(string(k), "hugepages-") {
			continue
		}
		out[k] = v.DeepCopy()
	}
	return out
}
