package probe

import (
	"context"
	"regexp"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Identity says what cluster this profile is about. It never reads tokens or
// certificate data from the kubeconfig only context name, cluster name, and
// server URL.
type Identity struct {
	KubeContext string `json:"kubeContext,omitempty"`
	ClusterName string `json:"clusterName,omitempty"`
	Server      string `json:"server,omitempty"`
	ClusterUID  string `json:"clusterUID,omitempty"`
	Flavor      Flavor `json:"flavor"`
}

// Flavor is the detected distribution/branding of the cluster.
type Flavor struct {
	Name          string   `json:"name,omitempty"`          // k3s, kind, gke, eks, minikube, ...
	VersionSuffix string   `json:"versionSuffix,omitempty"` // e.g. "k3s1" from v1.35.5+k3s1
	ProviderID    string   `json:"providerID,omitempty"`
	Hints         []string `json:"hints,omitempty"` // e.g. flannel, k3s
	// Environment is the substrate the distribution runs on: "k3d" for k3s
	// in Docker, while Name stays "k3s" so checks keyed on the distribution
	// (Traefik defaults, local-path, +k3s1) still match.
	Environment string `json:"environment,omitempty"`
}

func (p *prober) probeIdentity(ctx context.Context) (Identity, []ProbeError) {
	id := Identity{
		KubeContext: p.in.KubeContext,
		ClusterName: p.in.ClusterName,
	}
	if p.in.RESTConfig != nil {
		id.Server = p.in.RESTConfig.Host
	}
	var errs []ProbeError
	ns, err := p.in.Client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		errs = append(errs, asProbeError("identity", "namespaces/kube-system", err))
	} else {
		id.ClusterUID = string(ns.UID)
	}
	return id, errs
}

var (
	suffixRe   = regexp.MustCompile(`\+([0-9A-Za-z.-]+)$`)
	knownCNIs  = []string{"antrea", "calico", "cilium", "flannel", "kube-ovn", "weave"}
	k3sMarkers = []string{"k3s", "rke2"}
)

// detectFlavor derives cluster flavor from the server version suffix and the
// first node's providerID / annotations / labels. Pure function of profile
// facts, run after all probes complete.
func detectFlavor(v VersionInfo, nodes []NodeInfo) Flavor {
	f := Flavor{}
	if s := v.Server.GitVersion; s != "" {
		if m := suffixRe.FindStringSubmatch(s); m != nil {
			f.VersionSuffix = m[1]
		}
	}
	if len(nodes) > 0 {
		n := nodes[0]
		f.ProviderID = n.ProviderID
		hints := map[string]bool{}
		for k := range n.Annotations {
			lk := strings.ToLower(k)
			for _, cni := range knownCNIs {
				if strings.Contains(lk, cni) {
					hints[cni] = true
				}
			}
			for _, m := range k3sMarkers {
				if strings.HasPrefix(lk, m+".io/") {
					hints[m] = true
				}
			}
		}
		for k := range n.Labels {
			lk := strings.ToLower(k)
			for _, m := range k3sMarkers {
				if strings.HasPrefix(lk, m+".io/") {
					hints[m] = true
				}
			}
		}
		// k3d runs k3s inside Docker containers: node names are k3d-*,
		// providerIDs are k3s://k3d-*, and k3s.io/node-args names the
		// serverlb sidecar (read from rawNodeArgs the allowlist strips
		// node-args from Annotations). Surface it as the environment, not
		// the name: the distribution is still k3s.
		if looksLikeK3d(&n) {
			hints["k3d"] = true
			f.Environment = "k3d"
		}
		for h := range hints {
			f.Hints = append(f.Hints, h)
		}
		sort.Strings(f.Hints)
	}
	f.Name = flavorName(f.VersionSuffix, f.ProviderID)
	if f.Name == "" && f.Environment == "k3d" {
		f.Name = "k3s"
	}
	return f
}

// looksLikeK3d reports whether a node's facts point at a k3d cluster.
func looksLikeK3d(n *NodeInfo) bool {
	if strings.HasPrefix(n.Name, "k3d-") {
		return true
	}
	if strings.Contains(n.ProviderID, "k3d-") {
		return true
	}
	return strings.Contains(n.rawNodeArgs, "k3d-")
}

func flavorName(suffix, providerID string) string {
	combined := strings.ToLower(suffix + " " + providerID)
	// k3d is deliberately absent: its providerIDs read k3s://k3d-… and its
	// suffix is +k3s1, so the distribution is k3d only as an environment
	// (Flavor.Environment), never here.
	for _, known := range []string{"k3s", "rke2", "kind", "minikube", "gke", "eks", "aks", "k0s"} {
		if strings.Contains(combined, known) {
			return known
		}
	}
	if suffix != "" {
		return suffix
	}
	if i := strings.Index(providerID, "://"); i > 0 {
		return providerID[:i]
	}
	return ""
}
