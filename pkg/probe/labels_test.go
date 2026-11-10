package probe

import "testing"

func TestAllowMapKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{key: "kubernetes.io/hostname", want: true},
		{key: "node-role.kubernetes.io/control-plane", want: true},
		{key: "topology.kubernetes.io/zone", want: true},
		{key: "cilium.io/clustermesh", want: true},
		{key: "projectcalico.org/ipv4-addr", want: true},
		{key: "k3d.io/node-role", want: true},
		{key: "k3s.io/hostname", want: true},
		{key: "k3s.io/internal-ip", want: true},
		{key: "k3s.io/node-args", want: false},
		{key: "k3s.io/node-env", want: false},
		{key: "flannel.alpha.coreos.com/backend-data", want: false},
		{key: "flannel.alpha.coreos.com/public-ip", want: true},
		{key: "my-custom.io/foo", want: false},
		{key: "hostname", want: false},
		{key: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := allowMapKey(tc.key); got != tc.want {
				t.Errorf("allowMapKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestAllowMap(t *testing.T) {
	if got := allowMap(nil); got != nil {
		t.Errorf("allowMap(nil) = %v, want nil", got)
	}
	if got := allowMap(map[string]string{}); got != nil {
		t.Errorf("allowMap(empty) = %v, want nil", got)
	}
	allDropped := allowMap(map[string]string{"custom/key": "v", "other": "w"})
	if allDropped != nil {
		t.Errorf("all-dropped map = %v, want nil", allDropped)
	}
	in := map[string]string{
		"topology.kubernetes.io/zone":           "us-east-1a",
		"kubernetes.io/hostname":                "node-1",
		"flannel.alpha.coreos.com/backend-data": `{"vtepMAC":"aa:bb"}`,
		"custom.io/keep":                        "no",
	}
	want := map[string]string{
		"topology.kubernetes.io/zone": "us-east-1a",
		"kubernetes.io/hostname":      "node-1",
	}
	got := allowMap(in)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %q, want %q", k, got[k], v)
		}
	}
}
