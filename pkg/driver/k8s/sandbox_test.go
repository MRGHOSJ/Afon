package k8s

import "testing"

// TestIsLocalSandbox pins the allowlist: local sandbox prefixes match,
// production-shaped and malformed names never do, and the empty name
// never does.
func TestIsLocalSandbox(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"k3d-dev", true},
		{"kind-dev", true},
		{"minikube", true},
		{"rancher-desktop", true},
		{"docker-desktop", true},
		{"", false},
		{"prod", false},
		{"gke_prod_us", false},
		{"k3d", false},         // prefix needs the dash
		{"staging-k3d", false}, // prefix must lead
		{"kindness", false},    // kind- needs the dash
	}
	for _, tc := range cases {
		if got := IsLocalSandbox(tc.name); got != tc.want {
			t.Errorf("IsLocalSandbox(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
