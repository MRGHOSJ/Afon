package probe

import "testing"

func TestRedactURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{
			name: "userinfo host and path",
			raw:  "https://admin:pass@192.168.1.10:6443/api/v1",
			want: "https://redacted/api/v1",
		},
		{name: "no userinfo", raw: "http://10.0.0.1:8080", want: "http://redacted"},
		{name: "query kept", raw: "https://host.example/path?a=b", want: "https://redacted/path?a=b"},
		{name: "unparseable", raw: "http://exa mple.com", want: "redacted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactURL(tc.raw); got != tc.want {
				t.Errorf("redactURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestRedactString(t *testing.T) {
	if got := redactString(""); got != "" {
		t.Errorf("redactString(empty) = %q, want empty", got)
	}
	if got := redactString("k3d-dev"); got != "redacted" {
		t.Errorf("redactString = %q, want redacted", got)
	}
}

func TestRedactProviderHost(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{id: "aws:///i-0abc123", want: "aws://redacted"},
		{id: "gce://project/zone/name", want: "gce://redacted"},
		{id: "", want: ""},
		{id: "bare-provider", want: "bare-provider"},
		{id: "://leading", want: "://leading"},
	}
	for _, tc := range cases {
		if got := redactProviderHost(tc.id); got != tc.want {
			t.Errorf("redactProviderHost(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestMapNodeName(t *testing.T) {
	names := map[string]string{"node-a": "node-0"}
	if got := mapNodeName("node-a", names); got != "node-0" {
		t.Errorf("mapped = %q, want node-0", got)
	}
	if got := mapNodeName("stranger", names); got != "redacted" {
		t.Errorf("unmapped = %q, want redacted", got)
	}
}

func TestSensitiveKey(t *testing.T) {
	yes := []string{
		"cluster.x-k8s.io/token",
		"TOKEN",
		"my-password",
		"a-secret-value",
		"credentials",
		"k3s.io/node-env",
		"monkey", // substring match is documented behavior
	}
	no := []string{
		"topology.kubernetes.io/zone",
		"node.kubernetes.io/instance-type",
		"custom.example.com/note",
		"",
	}
	for _, k := range yes {
		if !sensitiveKey(k) {
			t.Errorf("sensitiveKey(%q) = false, want true", k)
		}
	}
	for _, k := range no {
		if sensitiveKey(k) {
			t.Errorf("sensitiveKey(%q) = true, want false", k)
		}
	}
}

func TestMaskIPs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "ipv4", in: "192.168.0.1", want: "redacted"},
		{name: "ipv4 with port", in: "192.168.0.1:8080", want: "redacted"},
		{name: "ipv4 in sentence", in: "reachable at 10.0.0.1 now", want: "reachable at redacted now"},
		{name: "compressed ipv6", in: "2001:db8::1", want: "redacted"},
		// The leading \b before "::" needs a word char first, so a bare
		// loopback literal at the start of a string is left alone today.
		{name: "bare loopback ipv6", in: "::1", want: "::1"},
		{name: "ipv6 after word", in: "at fe80::1", want: "at redacted"},
		{name: "link local ipv6", in: "fe80::1", want: "redacted"},
		{name: "full ipv6", in: "2001:0db8:85a3:0000:0000:8a2e:0370:7334", want: "redacted"},
		{name: "mixed", in: "node at 10.0.0.1 and fe80::2", want: "node at redacted and redacted"},
		{name: "timestamp is not an address", in: "12:34:56", want: "12:34:56"},
		{name: "version string untouched", in: "v1.35.5+k3s1", want: "v1.35.5+k3s1"},
		{name: "plain text", in: "no addresses here", want: "no addresses here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskIPs(tc.in); got != tc.want {
				t.Errorf("maskIPs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	p := &ClusterProfile{
		Identity: Identity{
			KubeContext: "k3d-dev",
			ClusterName: "k3d-cluster",
			Server:      "https://admin:pass@192.168.1.10:6443/api/v1",
			ClusterUID:  "uid-123",
			Flavor:      Flavor{Name: "k3s", ProviderID: "aws:///i-0abc123"},
		},
		Namespace: NamespaceInfo{UID: "ns-uid"},
		Nodes: NodesSection{Nodes: []NodeInfo{
			{
				Name:       "node-a",
				ProviderID: "aws:///i-1",
				PodCIDR:    "10.244.0.0/24",
				PodCIDRs:   []string{"10.244.0.0/24", "fd00::/64"},
				Addresses:  []AddressInfo{{Type: "InternalIP", Address: "10.0.0.5"}},
				Labels: map[string]string{
					"kubernetes.io/hostname":      "node-a",
					"topology.kubernetes.io/zone": "az1",
				},
				Annotations: map[string]string{
					"cluster.x-k8s.io/token":           "super-secret",
					"node.kubernetes.io/instance-type": "t3.medium",
					"custom.example.com/note":          "reachable at 192.168.1.7 or fe80::1",
					"custom.example.com/hostname":      "node-a",
				},
			},
			{
				Name:        "node-b",
				Addresses:   []AddressInfo{{Type: "ExternalIP", Address: "203.0.113.5"}},
				Labels:      map[string]string{"kubernetes.io/hostname": "node-b"},
				Annotations: map[string]string{},
			},
		}},
		Errors: []ProbeError{
			{Section: "nodes", Resource: "10.1.2.3:8080", Reason: "timeout talking to 10.1.2.3"},
		},
	}

	Redact(p)

	if got, want := p.Identity.Server, "https://redacted/api/v1"; got != want {
		t.Errorf("Server = %q, want %q", got, want)
	}
	for field, got := range map[string]string{
		"KubeContext":   p.Identity.KubeContext,
		"ClusterName":   p.Identity.ClusterName,
		"ClusterUID":    p.Identity.ClusterUID,
		"Namespace.UID": p.Namespace.UID,
	} {
		if got != "redacted" {
			t.Errorf("%s = %q, want redacted", field, got)
		}
	}
	if got, want := p.Identity.Flavor.ProviderID, "aws://redacted"; got != want {
		t.Errorf("Identity ProviderID = %q, want %q", got, want)
	}
	if p.Identity.Flavor.Name != "k3s" {
		t.Errorf("Flavor.Name = %q, want k3s (not redacted)", p.Identity.Flavor.Name)
	}

	n0 := p.Nodes.Nodes[0]
	if n0.Name != "node-0" {
		t.Errorf("node name = %q, want node-0", n0.Name)
	}
	if got, want := n0.ProviderID, "aws://redacted"; got != want {
		t.Errorf("node ProviderID = %q, want %q", got, want)
	}
	if n0.PodCIDR != "redacted" {
		t.Errorf("PodCIDR = %q, want redacted", n0.PodCIDR)
	}
	for i, cidr := range n0.PodCIDRs {
		if cidr != "redacted" {
			t.Errorf("PodCIDRs[%d] = %q, want redacted", i, cidr)
		}
	}
	if n0.Addresses[0].Address != "redacted" {
		t.Errorf("address = %q, want redacted", n0.Addresses[0].Address)
	}
	if got, want := n0.Labels["kubernetes.io/hostname"], "node-0"; got != want {
		t.Errorf("hostname label = %q, want %q", got, want)
	}
	if got, want := n0.Labels["topology.kubernetes.io/zone"], "az1"; got != want {
		t.Errorf("zone label = %q, want %q", got, want)
	}
	if got, want := n0.Annotations["cluster.x-k8s.io/token"], "redacted"; got != want {
		t.Errorf("token annotation = %q, want %q", got, want)
	}
	if got, want := n0.Annotations["node.kubernetes.io/instance-type"], "t3.medium"; got != want {
		t.Errorf("instance-type annotation = %q, want %q", got, want)
	}
	if got, want := n0.Annotations["custom.example.com/note"], "reachable at redacted or redacted"; got != want {
		t.Errorf("note annotation = %q, want %q", got, want)
	}
	if got, want := n0.Annotations["custom.example.com/hostname"], "node-0"; got != want {
		t.Errorf("hostname annotation = %q, want %q", got, want)
	}

	n1 := p.Nodes.Nodes[1]
	if n1.Name != "node-1" {
		t.Errorf("second node name = %q, want node-1", n1.Name)
	}
	if n1.ProviderID != "" {
		t.Errorf("second node ProviderID = %q, want empty", n1.ProviderID)
	}
	if got, want := n1.Labels["kubernetes.io/hostname"], "node-1"; got != want {
		t.Errorf("second hostname label = %q, want %q", got, want)
	}

	if got, want := p.Errors[0].Reason, "timeout talking to redacted"; got != want {
		t.Errorf("error reason = %q, want %q", got, want)
	}
	if got, want := p.Errors[0].Resource, "redacted"; got != want {
		t.Errorf("error resource = %q, want %q", got, want)
	}
}

func TestRedactEmptyProfile(t *testing.T) {
	p := &ClusterProfile{}
	Redact(p)
	if p.Identity.Server != "" || p.Identity.KubeContext != "" ||
		p.Identity.ClusterName != "" || p.Identity.ClusterUID != "" ||
		p.Namespace.UID != "" {
		t.Errorf("empty strings must stay empty: %+v", p.Identity)
	}
	if len(p.Nodes.Nodes) != 0 || len(p.Errors) != 0 {
		t.Errorf("empty profile gained entries: nodes=%d errors=%d", len(p.Nodes.Nodes), len(p.Errors))
	}
}
