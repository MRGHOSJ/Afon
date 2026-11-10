package probe

import "strings"

// mapKeyAllow are the label/annotation key prefixes worth keeping in a
// profile. Everything else is dropped at probe time so an unknown cluster's
// annotations (tokens, env dumps, config hashes) never reach the output.
var mapKeyAllow = []string{
	// Kubernetes metadata
	"kubernetes.io/",
	"beta.kubernetes.io/",
	"node.kubernetes.io/",
	"node-role.kubernetes.io/",
	"topology.kubernetes.io/",
	"failure-domain.beta.kubernetes.io/",
	"storage.kubernetes.io/",
	"pod-security.kubernetes.io/",
	// CNI / network plugins
	"flannel.alpha.coreos.com/",
	"cilium.io/",
	"projectcalico.org/",
	"canal.projectcalico.org/",
	"antrea.io/",
	"kubeovn.io/",
	"weave-net/",
	// k3d (k3s.io is NOT prefix-allowed see mapKeyExact)
	"k3d.io/",
}

// mapKeyExact allows individual keys under prefixes that are otherwise
// dropped. k3s.io/ qualifies: node-args carries the server's full command
// line (tokens included) and node-env carries K3S_TOKEN, so the prefix can
// never be a blanket allow only the harmless identity keys.
var mapKeyExact = []string{
	"k3s.io/hostname",
	"k3s.io/internal-ip",
}

// mapKeyDeny punches holes in the prefix allowlist: backend-data is a
// VtepMAC blob nothing downstream reads.
var mapKeyDeny = []string{
	"flannel.alpha.coreos.com/backend-data",
}

func allowMapKey(k string) bool {
	for _, d := range mapKeyDeny {
		if k == d {
			return false
		}
	}
	for _, e := range mapKeyExact {
		if k == e {
			return true
		}
	}
	for _, p := range mapKeyAllow {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// allowMap keeps only allowlisted keys; a map with nothing left becomes nil
// (omitted from JSON) instead of an empty object.
func allowMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		if allowMapKey(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
