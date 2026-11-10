package main

import (
	"strings"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/probe"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// testProfile is the rich fixture every render test starts from: one of each
// branch (default classes, gateway present, namespace exists, rules complete,
// readyz clean) so the golden text exercises the happy path end to end.
func testProfile() probe.ClusterProfile {
	created := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	return probe.ClusterProfile{
		SchemaVersion: probe.SchemaVersion,
		DurationMs:    1234,
		Identity: probe.Identity{
			KubeContext: "alpha",
			Flavor:      probe.Flavor{Name: "k3s", Environment: "k3d"},
		},
		Version: probe.VersionInfo{Server: probe.ServerVersionInfo{GitVersion: "v1.35.5+k3s1"}},
		Discovery: probe.DiscoveryInfo{
			GroupCount:    42,
			ResourceCount: 300,
			Capabilities: map[string]bool{
				"traefik": true, "metricsServer": true, "gatewayAPI": true, "k3sHelm": true,
				"certManager": false,
			},
			Full: []probe.GroupDetail{{
				GroupSummary: probe.GroupSummary{Name: "", PreferredVersion: "v1"},
				Resources:    []probe.ResourceDetail{{Name: "pods"}, {Name: "services"}},
			}},
		},
		Health: probe.HealthInfo{
			Ready:   true,
			CoreDNS: probe.CoreDNSInfo{Desired: 2, Ready: 2},
			Components: []probe.ComponentInfo{
				{Name: "coredns", Kind: "Deployment", Namespace: "kube-system"},
				{Name: "traefik", Kind: "Deployment", Namespace: "kube-system"},
				{Name: "metrics-server", Kind: "Deployment", Namespace: "kube-system"},
				{Name: "local-path-provisioner", Kind: "Deployment", Namespace: "kube-system"},
				{Name: "svclb", Kind: "DaemonSet", Namespace: "kube-system"},
			},
		},
		Nodes: probe.NodesSection{
			NodeCount: 2,
			Nodes: []probe.NodeInfo{
				{Conditions: []probe.ConditionInfo{{Type: "Ready", Status: "True"}}},
				{Conditions: []probe.ConditionInfo{{Type: "Ready", Status: "False"}}},
			},
			Capacity: probe.CapacitySummary{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("110"),
				},
				Requested: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
					corev1.ResourcePods:   resource.MustParse("10"),
				},
				SchedulingHeadroom: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("3"),
					corev1.ResourceMemory: resource.MustParse("6Gi"),
					corev1.ResourcePods:   resource.MustParse("100"),
				},
				ActivePods: 10,
			},
		},
		Access: probe.AccessInfo{
			Namespace: "team-a",
			Rules:     []probe.PolicyRuleInfo{{Verbs: []string{"list"}}},
			Checks: []probe.AccessCheck{
				{Resource: "pods", Verb: "list", Allowed: true},
				{Resource: "pods", Verb: "create", Allowed: true},
				{Resource: "secrets", Verb: "get", Allowed: true},
				{Resource: "secrets", Verb: "create", Allowed: false, Reason: "denied by RBAC"},
			},
		},
		Namespace: probe.NamespaceInfo{
			Exists:    true,
			Name:      "team-a",
			Phase:     "Active",
			CreatedAt: &created,
		},
		Networking: probe.NetworkingInfo{
			IngressClasses: []probe.IngressClassInfo{
				{Name: "traefik", IsDefault: false},
				{Name: "nginx", IsDefault: true},
			},
			GatewayAPI: &probe.GatewayAPIInfo{
				Present:    true,
				Classes:    []probe.GatewayClassInfo{{Name: "gc-a"}},
				Gateways:   []probe.GatewayInfo{{Name: "gw-1"}, {Name: "gw-2"}},
				HTTPRoutes: []probe.HTTPRouteInfo{{Name: "r-1"}, {Name: "r-2"}, {Name: "r-3"}},
			},
			NetworkPolicies: []probe.NetworkPolicyInfo{{Name: "deny-all"}, {Name: "allow-web"}},
			CNIHints:        []string{"flannel"},
		},
		Storage: probe.StorageInfo{
			StorageClasses: []probe.StorageClassInfo{
				{Name: "local-path", IsDefault: true},
				{Name: "fast", IsDefault: false},
			},
			PersistentVolumes:      []probe.PVInfo{{Name: "pv-1"}, {Name: "pv-2"}, {Name: "pv-3"}},
			PersistentVolumeClaims: []probe.PVCInfo{{Name: "pvc-1"}, {Name: "pvc-2"}, {Name: "pvc-3"}, {Name: "pvc-4"}, {Name: "pvc-5"}},
		},
	}
}

// fullProfile layers the --full extras onto the rich fixture: failing readyz
// text, structured issues, and a zero duration so the probe line is absent.
func fullProfile() probe.ClusterProfile {
	p := testProfile()
	p.DurationMs = 0
	p.Health.Readyz = "[+]readyz ok\n[-]etcd"
	p.Errors = []probe.ProbeError{{
		Section: "networking", Resource: "networkpolicies",
		Reason: "forbidden by policy", Code: "Forbidden",
	}}
	p.Sections = map[string]probe.SectionStatus{
		"access": {Status: probe.StatusDenied},
		"nodes":  {Status: probe.StatusPartial, Reason: "pods forbidden"},
	}
	return p
}

// minimalProfile hits the fallback branches: ClusterName instead of context,
// unknown flavor, missing namespace, no gateway API, no classes, incomplete
// rules, not-ready health, no duration.
func minimalProfile() probe.ClusterProfile {
	return probe.ClusterProfile{
		Identity:  probe.Identity{ClusterName: "prod"},
		Namespace: probe.NamespaceInfo{Name: "team-b"},
		Access:    probe.AccessInfo{RulesIncomplete: true},
	}
}

const goldenDefault = `cluster    alpha  ·  k3s (k3d)  ·  v1.35.5+k3s1
discovery  42 groups  ·  300 resources
detected   traefik, metrics-server, gateway-api, k3s-helm  ·  absent: cert-manager, argo-cd, flux, kyverno, vpa
nodes      2  ·  1 ready  ·  cpu 4 alloc / 1 req / 3 headroom  ·  mem 8Gi alloc / 2Gi req / 6Gi headroom  ·  pods 10 active / 100 headroom
namespace  team-a (Active)  ·  created 2026-03-01T12:00:00Z
networking cni flannel  ·  ingress-classes nginx (default), traefik  ·  gateway-api yes (1 classes, 2 gateways, 3 routes)  ·  netpols 2
storage    classes fast, local-path (default)  ·  pv 3  ·  pvc 5
access     3/4 checks allowed  ·  rules review complete
health     ready  ·  coredns 2/2  ·  5 components  ·  readyz ok
issues     none
probe      1234ms
`

const goldenFull = `cluster    alpha  ·  k3s (k3d)  ·  v1.35.5+k3s1
discovery  42 groups  ·  300 resources
detected   traefik, metrics-server, gateway-api, k3s-helm  ·  absent: cert-manager, argo-cd, flux, kyverno, vpa
nodes      2  ·  1 ready  ·  cpu 4 alloc / 1 req / 3 headroom  ·  mem 8Gi alloc / 2Gi req / 6Gi headroom  ·  pods 10 active / 100 headroom
namespace  team-a (Active)  ·  created 2026-03-01T12:00:00Z
networking cni flannel  ·  ingress-classes nginx (default), traefik  ·  gateway-api yes (1 classes, 2 gateways, 3 routes)  ·  netpols 2
storage    classes fast, local-path (default)  ·  pv 3  ·  pvc 5
access     3/4 checks allowed  ·  rules review complete
health     ready  ·  coredns 2/2  ·  5 components  ·  readyz raw below
issues     networking/networkpolicies: forbidden by policy
           section access denied
           section nodes partial: pods forbidden
readyz     raw:
    [+]readyz ok
    [-]etcd
apis       full:
    core (v1): pods, services
`

const goldenMinimal = `cluster    prod  ·  unknown  ·  -
discovery  0 groups  ·  0 resources
detected   none  ·  absent: cert-manager, argo-cd, flux, kyverno, vpa
nodes      0  ·  0 ready  ·  cpu - alloc / - req / - headroom  ·  mem - alloc / - req / - headroom  ·  pods 0 active / - headroom
namespace  team-b  ·  not found
networking ingress-classes none  ·  gateway-api no  ·  netpols 0
storage    classes none  ·  pv 0  ·  pvc 0
access     0/0 checks allowed  ·  rules review incomplete
health     not ready  ·  coredns 0/0  ·  0 components  ·  readyz ok
issues     none
`

func TestRenderSummaryGolden(t *testing.T) {
	tests := []struct {
		name    string
		profile probe.ClusterProfile
		full    bool
		want    string
	}{
		{"default", testProfile(), false, goldenDefault},
		{"full", fullProfile(), true, goldenFull},
		{"minimal", minimalProfile(), false, goldenMinimal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderSummary(tt.profile, tt.full)
			if got != tt.want {
				t.Errorf("renderSummary() mismatch\ngot:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// TestRenderSummaryFailingReadyzDefault covers the default-mode branch that
// keeps the failed readyz lines inline so a broken control plane is visible
// without --full.
func TestRenderSummaryFailingReadyzDefault(t *testing.T) {
	p := testProfile()
	p.Health.Readyz = "[-]etcd\n[-]kubelet"
	out := renderSummary(p, false)
	want := "health     ready  ·  coredns 2/2  ·  5 components  ·  readyz failing\n    [-]etcd\n    [-]kubelet\n"
	if !strings.Contains(out, want) {
		t.Errorf("renderSummary() missing failing-readyz block, got:\n%s", out)
	}
	if strings.Contains(out, "readyz     raw:") {
		t.Errorf("renderSummary() printed readyz raw block in default mode:\n%s", out)
	}
	if strings.Contains(out, "apis       full:") {
		t.Errorf("renderSummary() printed apis full block in default mode:\n%s", out)
	}
}

func TestClassList(t *testing.T) {
	tests := []struct {
		name    string
		classes []classInfo
		want    string
	}{
		{"empty", nil, "none"},
		{"sorted with default marked", []classInfo{{name: "z"}, {name: "a", isDefault: true}}, "a (default), z"},
		{"plain join", []classInfo{{name: "b"}, {name: "a"}}, "a, b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classList(tt.classes); got != tt.want {
				t.Errorf("classList() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCollectIssues(t *testing.T) {
	p := probe.ClusterProfile{
		Errors: []probe.ProbeError{{
			Section: "version", Resource: "serverversion", Reason: "boom",
		}},
		Sections: map[string]probe.SectionStatus{
			"nodes":    {Status: probe.StatusPartial, Reason: "pods forbidden"},
			"access":   {Status: probe.StatusDenied},
			"identity": {Status: probe.StatusOK},
		},
	}
	want := []string{
		"version/serverversion: boom",
		"section access denied",
		"section nodes partial: pods forbidden",
	}
	got := collectIssues(p)
	if len(got) != len(want) {
		t.Fatalf("collectIssues() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("collectIssues()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCollectIssuesEmpty(t *testing.T) {
	if got := collectIssues(probe.ClusterProfile{}); len(got) != 0 {
		t.Errorf("collectIssues() = %v, want none", got)
	}
}

func TestShorten(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short unchanged", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"newlines collapsed", "a\nb\nc", 10, "a b c"},
		{"truncated", "abcdefghij", 4, "abcd..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shorten(tt.in, tt.n); got != tt.want {
				t.Errorf("shorten(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestOrDash(t *testing.T) {
	if got := orDash(""); got != "-" {
		t.Errorf("orDash(\"\") = %q, want \"-\"", got)
	}
	if got := orDash("x"); got != "x" {
		t.Errorf("orDash(\"x\") = %q, want \"x\"", got)
	}
}

func TestTs(t *testing.T) {
	if got := ts(nil); got != "-" {
		t.Errorf("ts(nil) = %q, want \"-\"", got)
	}
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if got := ts(&at); got != "2026-03-01T12:00:00Z" {
		t.Errorf("ts() = %q, want RFC3339 UTC", got)
	}
	// A non-UTC instant must render in UTC, not local.
	plus := time.FixedZone("UTC+2", 2*3600)
	other := time.Date(2026, 3, 1, 14, 0, 0, 0, plus)
	if got := ts(&other); got != "2026-03-01T12:00:00Z" {
		t.Errorf("ts() = %q, want 2026-03-01T12:00:00Z", got)
	}
}

func TestResourceFormatters(t *testing.T) {
	t.Run("cpuAt", func(t *testing.T) {
		if got := cpuAt(nil); got != "-" {
			t.Errorf("cpuAt(nil) = %q, want \"-\"", got)
		}
		rl := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}
		if got := cpuAt(rl); got != "4" {
			t.Errorf("cpuAt(4) = %q, want \"4\"", got)
		}
		rl[corev1.ResourceCPU] = resource.MustParse("500m")
		if got := cpuAt(rl); got != "500m" {
			t.Errorf("cpuAt(500m) = %q, want \"500m\"", got)
		}
	})
	t.Run("memAt", func(t *testing.T) {
		if got := memAt(nil); got != "-" {
			t.Errorf("memAt(nil) = %q, want \"-\"", got)
		}
		rl := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")}
		if got := memAt(rl); got != "8Gi" {
			t.Errorf("memAt(8Gi) = %q, want \"8Gi\"", got)
		}
	})
	t.Run("podsAt", func(t *testing.T) {
		if got := podsAt(nil); got != "-" {
			t.Errorf("podsAt(nil) = %q, want \"-\"", got)
		}
		rl := corev1.ResourceList{corev1.ResourcePods: resource.MustParse("100")}
		if got := podsAt(rl); got != "100" {
			t.Errorf("podsAt(100) = %q, want \"100\"", got)
		}
	})
}

func TestHumanCPU(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"0", "0"},
		{"2", "2"},
		{"500m", "500m"},
		{"1500m", "1.5"},
		{"2500m", "2.5"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := humanCPU(resource.MustParse(tt.in)); got != tt.want {
				t.Errorf("humanCPU(%s) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"0", "0"},
		{"512", "512B"},
		{"2Ki", "2Ki"},
		{"5Mi", "5Mi"},
		{"1536Mi", "1.5Gi"},
		{"3Ti", "3Ti"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := humanBytes(resource.MustParse(tt.in)); got != tt.want {
				t.Errorf("humanBytes(%s) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTrimFloat(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{2, "2"},
		{1.5, "1.5"},
		{10, "10"},
	}
	for _, tt := range tests {
		if got := trimFloat(tt.in); got != tt.want {
			t.Errorf("trimFloat(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWriteDetected(t *testing.T) {
	tests := []struct {
		name string
		caps map[string]bool
		want string
	}{
		{
			"mixed",
			map[string]bool{
				"traefik": true, "metricsServer": true, "gatewayAPI": true, "k3sHelm": true,
				"certManager": false,
			},
			"detected   traefik, metrics-server, gateway-api, k3s-helm  ·  absent: cert-manager, argo-cd, flux, kyverno, vpa\n",
		},
		{
			"nothing to report",
			map[string]bool{
				"certManager": true, "argoCD": true, "flux": true, "kyverno": true, "vpa": true,
			},
			"",
		},
		{
			"all absent",
			map[string]bool{},
			"detected   none  ·  absent: cert-manager, argo-cd, flux, kyverno, vpa\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			writeDetected(&b, tt.caps)
			if got := b.String(); got != tt.want {
				t.Errorf("writeDetected() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCapabilityPresent(t *testing.T) {
	tests := []struct {
		name string
		caps map[string]bool
		key  string
		want bool
	}{
		{"direct hit", map[string]bool{"traefik": true}, "traefik", true},
		{"missing", map[string]bool{}, "traefik", false},
		{"flux via source", map[string]bool{"fluxSource": true}, "flux", true},
		{"flux via toolkit", map[string]bool{"flux": true}, "flux", true},
		{"flux neither", map[string]bool{}, "flux", false},
		{"flux unrelated key ignored", map[string]bool{"fluxSource": true}, "traefik", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := capabilityPresent(tt.caps, tt.key); got != tt.want {
				t.Errorf("capabilityPresent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJoinOrNone(t *testing.T) {
	if got := joinOrNone(nil); got != "none" {
		t.Errorf("joinOrNone(nil) = %q, want \"none\"", got)
	}
	if got := joinOrNone([]string{"a", "b"}); got != "a, b" {
		t.Errorf("joinOrNone() = %q, want \"a, b\"", got)
	}
}
