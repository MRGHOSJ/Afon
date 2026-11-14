package k8s

import "strings"

// localSandboxPrefixes are kubeconfig context names that address a
// throwaway local cluster — k3d and kind nodes, minikube, and the
// Docker/Rancher Desktop VMs.
var localSandboxPrefixes = []string{
	"k3d-",
	"kind-",
	"minikube",
	"rancher-desktop",
	"docker-desktop",
}

// IsLocalSandbox reports whether a kubeconfig context names a local
// sandbox cluster. The empty name is never a sandbox. It is a name
// heuristic for safety gates (tests deciding whether they may write to
// a cluster); production never gates on it — the driver applies
// wherever it is pointed, and the CLI banners the context either way.
func IsLocalSandbox(name string) bool {
	if name == "" {
		return false
	}
	for _, prefix := range localSandboxPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
