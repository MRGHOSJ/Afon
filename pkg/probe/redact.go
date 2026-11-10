package probe

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Redact scrubs identifying details from a profile so it can be committed as
// a fixture: server URL, kube context/cluster names, node names, IPs,
// providerID host part, and annotation values that look like secrets.
// It must never touch tokens or certificate material those are never read
// into the profile in the first place.
func Redact(p *ClusterProfile) {
	p.Identity.Server = redactURL(p.Identity.Server)
	p.Identity.KubeContext = redactString(p.Identity.KubeContext)
	p.Identity.ClusterName = redactString(p.Identity.ClusterName)
	p.Identity.ClusterUID = redactString(p.Identity.ClusterUID)
	p.Namespace.UID = redactString(p.Namespace.UID)
	if i := strings.Index(p.Identity.Flavor.ProviderID, "://"); i > 0 {
		p.Identity.Flavor.ProviderID = p.Identity.Flavor.ProviderID[:i+3] + "redacted"
	}

	nodeNames := map[string]string{}
	for i := range p.Nodes.Nodes {
		n := &p.Nodes.Nodes[i]
		if _, ok := nodeNames[n.Name]; !ok {
			nodeNames[n.Name] = fmt.Sprintf("node-%d", len(nodeNames))
		}
		n.Name = nodeNames[n.Name]
		n.ProviderID = redactProviderHost(n.ProviderID)
		n.PodCIDR = redactString(n.PodCIDR)
		for j := range n.PodCIDRs {
			n.PodCIDRs[j] = redactString(n.PodCIDRs[j])
		}
		for j := range n.Addresses {
			n.Addresses[j].Address = "redacted"
		}
		for k, v := range n.Labels {
			if strings.Contains(k, "hostname") {
				n.Labels[k] = mapNodeName(v, nodeNames)
			}
		}
		for k, v := range n.Annotations {
			switch {
			case sensitiveKey(k):
				n.Annotations[k] = "redacted"
			case strings.Contains(k, "hostname"):
				n.Annotations[k] = mapNodeName(v, nodeNames)
			default:
				n.Annotations[k] = maskIPs(v)
			}
		}
	}

	for i := range p.Errors {
		p.Errors[i].Reason = maskIPs(p.Errors[i].Reason)
		p.Errors[i].Resource = maskIPs(p.Errors[i].Resource)
	}
}

func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "redacted"
	}
	u.User = nil
	u.Host = "redacted"
	return u.String()
}

func redactString(s string) string {
	if s == "" {
		return ""
	}
	return "redacted"
}

func redactProviderHost(id string) string {
	if i := strings.Index(id, "://"); i > 0 {
		return id[:i+3] + "redacted"
	}
	return id
}

func mapNodeName(name string, nodeNames map[string]string) string {
	if mapped, ok := nodeNames[name]; ok {
		return mapped
	}
	return "redacted"
}

var sensitiveKeyRe = regexp.MustCompile(`(?i)(token|password|secret|credential|env|key)`)

func sensitiveKey(k string) bool {
	return sensitiveKeyRe.MatchString(k)
}

var ipv4Re = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b`)

// ipv6Re matches IPv6 literals: either the full 8-group form or an
// abbreviated form containing "::". Requiring "::" (or all 8 groups) keeps
// timestamps like 12:34:56 from being mistaken for addresses.
var ipv6Re = regexp.MustCompile(`(?i)(?:\b(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}\b|\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?\b|\b::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)*\b)`)

func maskIPs(s string) string {
	s = ipv4Re.ReplaceAllString(s, "redacted")
	return ipv6Re.ReplaceAllString(s, "redacted")
}
