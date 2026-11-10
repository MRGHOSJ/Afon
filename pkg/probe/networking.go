package probe

import (
	"context"
	"sort"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type IngressClassInfo struct {
	Name       string `json:"name"`
	Controller string `json:"controller,omitempty"`
	IsDefault  bool   `json:"isDefault"`
}

type GatewayClassInfo struct {
	Name       string `json:"name"`
	Controller string `json:"controller,omitempty"`
	Accepted   string `json:"accepted,omitempty"` // True / False / Unknown
}

type GatewayInfo struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
	Listeners int      `json:"listeners,omitempty"`
}

type HTTPRouteInfo struct {
	Name      string   `json:"name"`
	Hostnames []string `json:"hostnames,omitempty"`
	Gateways  []string `json:"gateways,omitempty"`
}

// GatewayAPI is nil when the CRDs are not installed an optional component.
// When present, classes/gateways/httproutes are always emitted: [] means
// probed-and-none, null means not probed (the error is in errors[]), so a
// consumer can tell "zero GatewayClasses" from "didn't look".
type GatewayAPIInfo struct {
	Present    bool               `json:"present"`
	Versions   []string           `json:"versions,omitempty"`
	Classes    []GatewayClassInfo `json:"classes"`
	Gateways   []GatewayInfo      `json:"gateways"`
	HTTPRoutes []HTTPRouteInfo    `json:"httproutes"`
}

type NetworkPolicyInfo struct {
	Name         string            `json:"name"`
	PodSelector  map[string]string `json:"podSelector,omitempty"`
	PolicyTypes  []string          `json:"policyTypes,omitempty"`
	IngressRules int               `json:"ingressRules"`
	EgressRules  int               `json:"egressRules"`
}

// NetworkingInfo holds routing and reachability facts for the target
// namespace plus cluster-level ingress capability.
type NetworkingInfo struct {
	IngressClasses  []IngressClassInfo  `json:"ingressClasses"`
	GatewayAPI      *GatewayAPIInfo     `json:"gatewayAPI"`
	NetworkPolicies []NetworkPolicyInfo `json:"networkPolicies"`
	CNIHints        []string            `json:"cniHints,omitempty"`
}

func (p *prober) probeNetworking(ctx context.Context) (NetworkingInfo, []ProbeError) {
	var info NetworkingInfo

	// IngressClasses, the Gateway API block, CNI hints and NetworkPolicies are
	// independent; run them concurrently and merge errors in the original
	// order (ingressclasses → gateway → cniHints → netpols) so classify() sees
	// the same errs[0] as before.
	var (
		wg         sync.WaitGroup
		classes    *networkingv1.IngressClassList
		classesErr *ProbeError
		gw         *GatewayAPIInfo
		gwErrs     []ProbeError
		hints      []string
		hintsErrs  []ProbeError
		pols       *networkingv1.NetworkPolicyList
		polsErr    *ProbeError
	)

	wg.Add(4)
	go func() {
		defer wg.Done()
		var err error
		classes, err = p.in.Client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("networking", "ingressclasses", err)
			classesErr = &pe
		}
	}()
	go func() {
		defer wg.Done()
		gw, gwErrs = p.probeGatewayAPI(ctx)
	}()
	go func() {
		defer wg.Done()
		hints, hintsErrs = p.cniHints(ctx)
	}()
	go func() {
		defer wg.Done()
		var err error
		// Namespaced convention: NotFound means the target namespace does not
		// exist an empty policy list is the honest fact, not a failure.
		pols, err = p.in.Client.NetworkingV1().NetworkPolicies(p.in.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			pe := asProbeError("networking", "networkpolicies", err)
			polsErr = &pe
		}
	}()
	wg.Wait()

	var errs []ProbeError
	if classesErr != nil {
		errs = append(errs, *classesErr)
	} else {
		info.IngressClasses = []IngressClassInfo{}
		for i := range classes.Items {
			ic := &classes.Items[i]
			info.IngressClasses = append(info.IngressClasses, IngressClassInfo{
				Name:       ic.Name,
				Controller: ic.Spec.Controller,
				IsDefault:  ic.Annotations["ingressclass.kubernetes.io/is-default-class"] == "true",
			})
		}
		sort.Slice(info.IngressClasses, func(i, j int) bool {
			return info.IngressClasses[i].Name < info.IngressClasses[j].Name
		})
	}

	errs = append(errs, gwErrs...)
	info.GatewayAPI = gw

	errs = append(errs, hintsErrs...)
	info.CNIHints = hints

	if polsErr != nil {
		errs = append(errs, *polsErr)
	} else if pols != nil {
		info.NetworkPolicies = []NetworkPolicyInfo{}
		for i := range pols.Items {
			np := &pols.Items[i]
			info.NetworkPolicies = append(info.NetworkPolicies, NetworkPolicyInfo{
				Name:         np.Name,
				PodSelector:  np.Spec.PodSelector.MatchLabels,
				PolicyTypes:  policyTypes(np),
				IngressRules: len(np.Spec.Ingress),
				EgressRules:  len(np.Spec.Egress),
			})
		}
		sort.Slice(info.NetworkPolicies, func(i, j int) bool {
			return info.NetworkPolicies[i].Name < info.NetworkPolicies[j].Name
		})
	}
	return info, errs
}

func policyTypes(np *networkingv1.NetworkPolicy) []string {
	var out []string
	for _, t := range np.Spec.PolicyTypes {
		out = append(out, string(t))
	}
	return out
}

// probeGatewayAPI is discovery-gated: no CRDs → nil (fact), not an error.
// It returns its errors instead of appending to a shared slice so it can run
// concurrently with the other networking sub-probes.
func (p *prober) probeGatewayAPI(ctx context.Context) (*GatewayAPIInfo, []ProbeError) {
	var errs []ProbeError
	groups, err := p.in.Client.Discovery().ServerGroups()
	if err != nil {
		errs = append(errs, asProbeError("networking", "servergroups", err))
		return nil, errs
	}
	var gwGroup = "gateway.networking.k8s.io"
	var versions []string
	var preferred string
	found := false
	for _, g := range groups.Groups {
		if g.Name == gwGroup {
			found = true
			for _, v := range g.Versions {
				versions = append(versions, v.Version)
			}
			preferred = g.PreferredVersion.Version
		}
	}
	if !found {
		return nil, errs
	}
	if p.in.Dynamic == nil {
		errs = append(errs, ProbeError{
			Section: "networking", Resource: gwGroup,
			Reason: "dynamic client not configured", Code: "Error",
		})
		return &GatewayAPIInfo{Present: true, Versions: versions}, errs
	}
	out := &GatewayAPIInfo{Present: true, Versions: versions}

	gvr := schema.GroupVersionResource{Group: gwGroup, Version: preferred, Resource: "gatewayclasses"}
	gcs, err := p.in.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		errs = append(errs, asProbeError("networking", "gatewayclasses", err))
	} else {
		out.Classes = []GatewayClassInfo{}
		for i := range gcs.Items {
			gc := &gcs.Items[i]
			out.Classes = append(out.Classes, GatewayClassInfo{
				Name:       gc.GetName(),
				Controller: unstructuredString(gc, "spec", "controller"),
				Accepted:   conditionStatus(gc, "Accepted"),
			})
		}
		sort.Slice(out.Classes, func(i, j int) bool { return out.Classes[i].Name < out.Classes[j].Name })
	}

	ns := p.in.Namespace
	gtwGVR := schema.GroupVersionResource{Group: gwGroup, Version: preferred, Resource: "gateways"}
	gtws, err := p.in.Dynamic.Resource(gtwGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, asProbeError("networking", "gateways", err))
	} else {
		// NotFound (namespace missing) is an empty fact, like networkPolicies.
		out.Gateways = []GatewayInfo{}
		if err == nil {
			for i := range gtws.Items {
				gw := &gtws.Items[i]
				info := GatewayInfo{Name: gw.GetName()}
				if addrs, found, _ := unstructured.NestedSlice(gw.Object, "status", "addresses"); found {
					for _, a := range addrs {
						if m, ok := a.(map[string]any); ok {
							if v, ok := m["value"].(string); ok {
								info.Addresses = append(info.Addresses, v)
							}
						}
					}
				}
				if listeners, found, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners"); found {
					info.Listeners = len(listeners)
				}
				out.Gateways = append(out.Gateways, info)
			}
			sort.Slice(out.Gateways, func(i, j int) bool { return out.Gateways[i].Name < out.Gateways[j].Name })
		}
	}

	routeGVR := schema.GroupVersionResource{Group: gwGroup, Version: preferred, Resource: "httproutes"}
	routes, err := p.in.Dynamic.Resource(routeGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, asProbeError("networking", "httproutes", err))
	} else {
		// NotFound (namespace missing) is an empty fact, like networkPolicies.
		out.HTTPRoutes = []HTTPRouteInfo{}
		if err == nil {
			for i := range routes.Items {
				r := &routes.Items[i]
				info := HTTPRouteInfo{Name: r.GetName()}
				if hosts, found, _ := unstructured.NestedStringSlice(r.Object, "spec", "hostnames"); found {
					info.Hostnames = hosts
				}
				if parents, found, _ := unstructured.NestedSlice(r.Object, "spec", "parentRefs"); found {
					for _, pr := range parents {
						if m, ok := pr.(map[string]any); ok {
							if name, ok := m["name"].(string); ok {
								info.Gateways = append(info.Gateways, name)
							}
						}
					}
				}
				out.HTTPRoutes = append(out.HTTPRoutes, info)
			}
			sort.Slice(out.HTTPRoutes, func(i, j int) bool { return out.HTTPRoutes[i].Name < out.HTTPRoutes[j].Name })
		}
	}
	return out, errs
}

// cniHints detects the CNI from node annotations (flannel runs outside the
// pod network in k3s, so annotations are the reliable signal) and from
// kube-system DaemonSet names. It returns its errors instead of appending to
// a shared slice so it can run concurrently with the other networking
// sub-probes.
func (p *prober) cniHints(ctx context.Context) ([]string, []ProbeError) {
	seen := map[string]bool{}

	var (
		wg       sync.WaitGroup
		nodes    *corev1.NodeList
		nodesErr *ProbeError
		ds       *appsv1.DaemonSetList
		dsErr    *ProbeError
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		var err error
		nodes, err = p.in.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("networking", "nodes", err)
			nodesErr = &pe
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		ds, err = p.in.Client.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("networking", "daemonsets/kube-system", err)
			dsErr = &pe
		}
	}()
	wg.Wait()

	var errs []ProbeError
	if nodesErr != nil {
		errs = append(errs, *nodesErr)
	} else {
		for i := range nodes.Items {
			for k := range nodes.Items[i].Annotations {
				lk := strings.ToLower(k)
				for _, cni := range knownCNIs {
					if strings.Contains(lk, cni) {
						seen[cni] = true
					}
				}
			}
		}
	}
	if dsErr != nil {
		errs = append(errs, *dsErr)
	} else {
		for i := range ds.Items {
			ln := strings.ToLower(ds.Items[i].Name)
			for _, cni := range knownCNIs {
				if strings.Contains(ln, cni) {
					seen[cni] = true
				}
			}
		}
	}
	var hints []string
	for c := range seen {
		hints = append(hints, c)
	}
	sort.Strings(hints)
	return hints, errs
}

func unstructuredString(u *unstructured.Unstructured, fields ...string) string {
	v, found, _ := unstructured.NestedString(u.Object, fields...)
	if !found {
		return ""
	}
	return v
}

func conditionStatus(u *unstructured.Unstructured, condType string) string {
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return ""
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == condType {
			if s, ok := m["status"].(string); ok {
				return s
			}
		}
	}
	return ""
}
