package probe

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
)

type GroupSummary struct {
	Name             string   `json:"name"`
	Versions         []string `json:"versions,omitempty"`
	PreferredVersion string   `json:"preferredVersion,omitempty"`
	ResourceCount    int      `json:"resourceCount"`
}

type ResourceDetail struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind,omitempty"`
	Namespaced bool     `json:"namespaced"`
	Verbs      []string `json:"verbs,omitempty"`
}

type GroupDetail struct {
	GroupSummary
	Resources []ResourceDetail `json:"resources"`
}

// DiscoveryInfo summarises the API surface. Capabilities is a curated map of
// bools for groups the pipeline cares about; the full per-resource list is
// only included with Input.Full, because it makes profiles (and fixtures) big.
type DiscoveryInfo struct {
	GroupCount    int             `json:"groupCount"`
	ResourceCount int             `json:"resourceCount"`
	Groups        []GroupSummary  `json:"groups"`
	Capabilities  map[string]bool `json:"capabilities"`
	Full          []GroupDetail   `json:"full,omitempty"`
}

// capabilityGroups maps API group → capability name for groups that change
// what the pipeline does (optional components, policy engines, CNIs).
var capabilityGroups = []struct{ Group, Name string }{
	{"gateway.networking.k8s.io", "gatewayAPI"},
	{"cert-manager.io", "certManager"},
	{"argoproj.io", "argoCD"},
	{"kustomize.toolkit.fluxcd.io", "flux"},
	{"source.toolkit.fluxcd.io", "fluxSource"},
	{"kyverno.io", "kyverno"},
	{"gatekeeper.systems", "gatekeeper"},
	{"metrics.k8s.io", "metricsServer"},
	{"traefik.io", "traefik"},
	{"autoscaling.k8s.io", "vpa"},
	{"k3s.cattle.io", "k3s"},
	{"helm.cattle.io", "k3sHelm"},
}

func (p *prober) probeDiscovery(ctx context.Context) (DiscoveryInfo, []ProbeError) {
	groups, resources, err := p.in.Client.Discovery().ServerGroupsAndResources()
	var errs []ProbeError
	if err != nil {
		// Aggregated APIs fail discovery all the time (broken metrics-server
		// APIService is the classic case): keep the usable data and record
		// one structured error per failed group.
		if failed, ok := discovery.GroupDiscoveryFailedErrorGroups(err); ok {
			for gv, gerr := range failed {
				errs = append(errs, ProbeError{
					Section:  "discovery",
					Resource: gv.String(),
					Reason:   gerr.Error(),
					Code:     "PartialDiscovery",
				})
			}
		} else {
			errs = append(errs, asProbeError("discovery", "api-resources", err))
			if len(groups) == 0 && len(resources) == 0 {
				return DiscoveryInfo{}, errs
			}
		}
	}

	info := DiscoveryInfo{
		GroupCount:   len(groups),
		Capabilities: map[string]bool{},
	}
	for _, cap := range capabilityGroups {
		info.Capabilities[cap.Name] = false
	}

	resourceCountByGV := map[string]int{}
	for _, rl := range resources {
		resourceCountByGV[rl.GroupVersion] += len(rl.APIResources)
		info.ResourceCount += len(rl.APIResources)
	}

	groupNames := map[string]bool{}
	info.Groups = []GroupSummary{}
	for _, g := range groups {
		groupNames[g.Name] = true
		summary := GroupSummary{Name: g.Name, PreferredVersion: g.PreferredVersion.Version}
		for _, v := range g.Versions {
			summary.Versions = append(summary.Versions, v.Version)
		}
		summary.ResourceCount = resourceCountByGV[g.PreferredVersion.GroupVersion]
		info.Groups = append(info.Groups, summary)
		for _, cap := range capabilityGroups {
			if cap.Group == g.Name {
				info.Capabilities[cap.Name] = true
			}
		}
	}
	sort.Slice(info.Groups, func(i, j int) bool { return info.Groups[i].Name < info.Groups[j].Name })

	if p.in.Full {
		info.Full = groupDetails(groups, resources)
	}
	return info, errs
}

func groupDetails(groups []*metav1.APIGroup, resources []*metav1.APIResourceList) []GroupDetail {
	details := []GroupDetail{}
	byGV := map[string][]ResourceDetail{}
	for _, rl := range resources {
		for _, r := range rl.APIResources {
			if strings.Contains(r.Name, "/") {
				continue // subresources like pods/log
			}
			byGV[rl.GroupVersion] = append(byGV[rl.GroupVersion], ResourceDetail{
				Name:       r.Name,
				Kind:       r.Kind,
				Namespaced: r.Namespaced,
				Verbs:      r.Verbs,
			})
		}
	}
	for _, g := range groups {
		d := GroupDetail{GroupSummary: GroupSummary{Name: g.Name, PreferredVersion: g.PreferredVersion.Version}}
		// A resource served under several versions appears once: the detail
		// records no version, so repeats would be indistinguishable.
		seen := map[string]bool{}
		for _, v := range g.Versions {
			d.Versions = append(d.Versions, v.Version)
			for _, r := range byGV[v.GroupVersion] {
				if seen[r.Name] {
					continue
				}
				seen[r.Name] = true
				d.Resources = append(d.Resources, r)
			}
		}
		sort.Slice(d.Resources, func(i, j int) bool { return d.Resources[i].Name < d.Resources[j].Name })
		d.ResourceCount = len(d.Resources)
		details = append(details, d)
	}
	sort.Slice(details, func(i, j int) bool { return details[i].Name < details[j].Name })
	return details
}
