package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MRGHOSJ/afon/pkg/probe"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// renderSummary is the default text view of a profile: a compact fact sheet
// mirroring the JSON, never a verdict. full mirrors Input.Full raw /readyz
// text and the complete API resource list. -o json emits the full profile.
func renderSummary(p probe.ClusterProfile, full bool) string {
	var b strings.Builder

	name := p.Identity.KubeContext
	if name == "" {
		name = p.Identity.ClusterName
	}
	flavor := p.Identity.Flavor.Name
	if flavor == "" {
		flavor = "unknown"
	}
	if env := p.Identity.Flavor.Environment; env != "" {
		flavor = fmt.Sprintf("%s (%s)", flavor, env)
	}
	fmt.Fprintf(&b, "cluster    %s  ·  %s  ·  %s\n",
		orDash(name), flavor, orDash(p.Version.Server.GitVersion))
	fmt.Fprintf(&b, "discovery  %d groups  ·  %d resources\n",
		p.Discovery.GroupCount, p.Discovery.ResourceCount)
	writeDetected(&b, p.Discovery.Capabilities)

	n := p.Nodes
	ready := 0
	for i := range n.Nodes {
		for _, c := range n.Nodes[i].Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
				break
			}
		}
	}
	c := n.Capacity
	fmt.Fprintf(&b, "nodes      %d  ·  %d ready  ·  cpu %s alloc / %s req / %s headroom  ·  mem %s alloc / %s req / %s headroom  ·  pods %d active / %s headroom\n",
		n.NodeCount, ready,
		cpuAt(c.Allocatable), cpuAt(c.Requested), cpuAt(c.SchedulingHeadroom),
		memAt(c.Allocatable), memAt(c.Requested), memAt(c.SchedulingHeadroom),
		c.ActivePods, podsAt(c.SchedulingHeadroom))

	ns := p.Namespace
	if ns.Exists {
		fmt.Fprintf(&b, "namespace  %s (%s)  ·  created %s\n",
			orDash(ns.Name), orDash(ns.Phase), ts(ns.CreatedAt))
	} else {
		fmt.Fprintf(&b, "namespace  %s  ·  not found\n", orDash(ns.Name))
	}

	netParts := []string{}
	if len(p.Networking.CNIHints) > 0 {
		netParts = append(netParts, "cni "+strings.Join(p.Networking.CNIHints, ", "))
	}
	icClasses := make([]classInfo, 0, len(p.Networking.IngressClasses))
	for _, ic := range p.Networking.IngressClasses {
		icClasses = append(icClasses, classInfo{name: ic.Name, isDefault: ic.IsDefault})
	}
	netParts = append(netParts, "ingress-classes "+classList(icClasses))
	switch g := p.Networking.GatewayAPI; {
	case g == nil:
		netParts = append(netParts, "gateway-api no")
	default:
		netParts = append(netParts, fmt.Sprintf("gateway-api yes (%d classes, %d gateways, %d routes)",
			len(g.Classes), len(g.Gateways), len(g.HTTPRoutes)))
	}
	netParts = append(netParts, fmt.Sprintf("netpols %d", len(p.Networking.NetworkPolicies)))
	fmt.Fprintf(&b, "networking %s\n", strings.Join(netParts, "  ·  "))

	scClasses := make([]classInfo, 0, len(p.Storage.StorageClasses))
	for _, sc := range p.Storage.StorageClasses {
		scClasses = append(scClasses, classInfo{name: sc.Name, isDefault: sc.IsDefault})
	}
	fmt.Fprintf(&b, "storage    classes %s  ·  pv %d  ·  pvc %d\n",
		classList(scClasses), len(p.Storage.PersistentVolumes), len(p.Storage.PersistentVolumeClaims))

	allowed := 0
	for _, chk := range p.Access.Checks {
		if chk.Allowed {
			allowed++
		}
	}
	rules := "complete"
	if p.Access.RulesIncomplete {
		rules = "incomplete"
	}
	fmt.Fprintf(&b, "access     %d/%d checks allowed  ·  rules review %s\n",
		allowed, len(p.Access.Checks), rules)

	h := p.Health
	readyStr := "not ready"
	if h.Ready {
		readyStr = "ready"
	}
	switch {
	case full && h.Readyz != "":
		fmt.Fprintf(&b, "health     %s  ·  coredns %d/%d  ·  %d components  ·  readyz raw below\n",
			readyStr, h.CoreDNS.Ready, h.CoreDNS.Desired, len(h.Components))
	case h.Readyz == "":
		fmt.Fprintf(&b, "health     %s  ·  coredns %d/%d  ·  %d components  ·  readyz ok\n",
			readyStr, h.CoreDNS.Ready, h.CoreDNS.Desired, len(h.Components))
	default:
		// Default mode keeps only failed readyz checks; show them here so a
		// failing control plane never reads as "issues none".
		fmt.Fprintf(&b, "health     %s  ·  coredns %d/%d  ·  %d components  ·  readyz failing\n",
			readyStr, h.CoreDNS.Ready, h.CoreDNS.Desired, len(h.Components))
		for _, line := range strings.Split(h.Readyz, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}

	issues := collectIssues(p)
	if len(issues) == 0 {
		b.WriteString("issues     none\n")
	} else {
		for i, issue := range issues {
			if i == 0 {
				fmt.Fprintf(&b, "issues     %s\n", issue)
			} else {
				fmt.Fprintf(&b, "           %s\n", issue)
			}
		}
	}
	if p.DurationMs > 0 {
		fmt.Fprintf(&b, "probe      %dms\n", p.DurationMs)
	}
	if full {
		if h.Readyz != "" {
			b.WriteString("readyz     raw:\n")
			for _, line := range strings.Split(h.Readyz, "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
		if len(p.Discovery.Full) > 0 {
			b.WriteString("apis       full:\n")
			for _, g := range p.Discovery.Full {
				group := g.Name
				if group == "" {
					group = "core"
				}
				names := make([]string, 0, len(g.Resources))
				for _, r := range g.Resources {
					names = append(names, r.Name)
				}
				fmt.Fprintf(&b, "    %s (%s): %s\n", group, orDash(g.PreferredVersion), strings.Join(names, ", "))
			}
		}
	}
	return b.String()
}

// detectedPresent / detectedAbsent are the pipeline-relevant components in
// fixed display order; keys map into DiscoveryInfo.Capabilities.
var detectedPresent = []struct{ label, key string }{
	{"traefik", "traefik"},
	{"metrics-server", "metricsServer"},
	{"gateway-api", "gatewayAPI"},
	{"k3s-helm", "k3sHelm"},
}

var detectedAbsent = []struct{ label, key string }{
	{"cert-manager", "certManager"},
	{"argo-cd", "argoCD"},
	{"flux", "flux"},
	{"kyverno", "kyverno"},
	{"vpa", "vpa"},
}

func writeDetected(b *strings.Builder, caps map[string]bool) {
	var present, absent []string
	for _, d := range detectedPresent {
		if capabilityPresent(caps, d.key) {
			present = append(present, d.label)
		}
	}
	for _, d := range detectedAbsent {
		if !capabilityPresent(caps, d.key) {
			absent = append(absent, d.label)
		}
	}
	if len(present) == 0 && len(absent) == 0 {
		return
	}
	line := "detected   " + joinOrNone(present)
	if len(absent) > 0 {
		line += "  ·  absent: " + strings.Join(absent, ", ")
	}
	b.WriteString(line + "\n")
}

// capabilityPresent reads DiscoveryInfo.Capabilities; flux spans two keys
// (source + kustomize toolkit) under one display name.
func capabilityPresent(caps map[string]bool, key string) bool {
	if key == "flux" {
		return caps["flux"] || caps["fluxSource"]
	}
	return caps[key]
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

type classInfo struct {
	name      string
	isDefault bool
}

// classList renders class names sorted, each default one marked, so a plan
// can reference them by name empty means none exist.
func classList(classes []classInfo) string {
	if len(classes) == 0 {
		return "none"
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].name < classes[j].name })
	labels := make([]string, len(classes))
	for i, c := range classes {
		labels[i] = c.name
		if c.isDefault {
			labels[i] += " (default)"
		}
	}
	return strings.Join(labels, ", ")
}

// collectIssues lists probe errors and non-ok sections in deterministic order.
func collectIssues(p probe.ClusterProfile) []string {
	var out []string
	for _, e := range p.Errors {
		res := e.Section
		if e.Resource != "" {
			res += "/" + e.Resource
		}
		out = append(out, res+": "+shorten(e.Reason, 120))
	}
	names := make([]string, 0, len(p.Sections))
	for name := range p.Sections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := p.Sections[name]
		if s.Status == probe.StatusOK {
			continue
		}
		line := "section " + name + " " + s.Status
		if s.Reason != "" {
			line += ": " + shorten(s.Reason, 120)
		}
		out = append(out, line)
	}
	return out
}

func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ts(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func cpuAt(rl corev1.ResourceList) string {
	if rl == nil {
		return "-"
	}
	return humanCPU(rl[corev1.ResourceCPU])
}

func memAt(rl corev1.ResourceList) string {
	if rl == nil {
		return "-"
	}
	return humanBytes(rl[corev1.ResourceMemory])
}

func podsAt(rl corev1.ResourceList) string {
	if rl == nil {
		return "-"
	}
	q := rl[corev1.ResourcePods]
	return fmt.Sprintf("%d", q.Value())
}

func humanCPU(q resource.Quantity) string {
	m := q.MilliValue()
	switch {
	case m == 0:
		return "0"
	case m%1000 == 0:
		return fmt.Sprintf("%d", m/1000)
	case m < 1000:
		return fmt.Sprintf("%dm", m)
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(m)/1000), ".0")
	}
}

func humanBytes(q resource.Quantity) string {
	b := q.Value()
	if b == 0 {
		return "0"
	}
	const (
		ki = 1 << 10
		mi = 1 << 20
		gi = 1 << 30
		ti = 1 << 40
	)
	switch {
	case b >= ti:
		return fmt.Sprintf("%sTi", trimFloat(float64(b)/ti))
	case b >= gi:
		return fmt.Sprintf("%sGi", trimFloat(float64(b)/gi))
	case b >= mi:
		return fmt.Sprintf("%sMi", trimFloat(float64(b)/mi))
	case b >= ki:
		return fmt.Sprintf("%sKi", trimFloat(float64(b)/ki))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func trimFloat(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0")
}
