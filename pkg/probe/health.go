package probe

import (
	"context"
	"sort"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ComponentInfo struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // Deployment or DaemonSet
	Namespace string   `json:"namespace"`
	Desired   int32    `json:"desired"`
	Ready     int32    `json:"ready"`
	Images    []string `json:"images,omitempty"`
}

type CoreDNSInfo struct {
	Desired int32 `json:"desired"`
	Ready   int32 `json:"ready"`
}

type HealthInfo struct {
	Ready      bool            `json:"ready"`
	Readyz     string          `json:"readyz,omitempty"` // failed checks only; raw verbose text with --full
	CoreDNS    CoreDNSInfo     `json:"coredns"`
	Components []ComponentInfo `json:"components"`
}

func (p *prober) probeHealth(ctx context.Context) (HealthInfo, []ProbeError) {
	var h HealthInfo

	// /readyz, deployments and daemonsets are independent round trips; run
	// them concurrently and assemble in the original order afterwards.
	var (
		wg            sync.WaitGroup
		readyzErr     *ProbeError
		readyOK       bool
		readyzBody    string
		deploys       *appsv1.DeploymentList
		deploysErr    *ProbeError
		daemonsets    *appsv1.DaemonSetList
		daemonsetsErr *ProbeError
	)

	wg.Add(3)
	go func() {
		defer wg.Done()
		// /readyz via the discovery REST client; the fake client's is nil, which
		// we record as unsupported rather than crashing.
		rc := p.in.Client.Discovery().RESTClient()
		if rc == nil {
			pe := ProbeError{
				Section: "health", Resource: "readyz",
				Reason: "discovery REST client not available", Code: "Unsupported",
			}
			readyzErr = &pe
			return
		}
		res := rc.Get().AbsPath("/readyz").Param("verbose", "true").Do(ctx)
		body, _ := res.Raw()
		var code int
		res.StatusCode(&code)
		readyOK = code == 200
		readyzBody = strings.TrimSpace(string(body))
		if err := res.Error(); err != nil {
			// A 500 from /readyz carries the failing checks in the body;
			// the error is recorded, the body is filtered at assembly.
			readyzErr = func() *ProbeError {
				pe := asProbeError("health", "readyz", err)
				return &pe
			}()
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		deploys, err = p.in.Client.AppsV1().Deployments("kube-system").List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("health", "deployments/kube-system", err)
			deploysErr = &pe
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		daemonsets, err = p.in.Client.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("health", "daemonsets/kube-system", err)
			daemonsetsErr = &pe
		}
	}()
	wg.Wait()

	var errs []ProbeError
	if readyzErr != nil {
		errs = append(errs, *readyzErr)
	}
	h.Ready = readyOK
	h.Readyz = truncate(readyzSummary(readyzBody, p.in.Full), 2048)

	if deploysErr != nil {
		errs = append(errs, *deploysErr)
	} else {
		h.Components = []ComponentInfo{}
		for i := range deploys.Items {
			d := &deploys.Items[i]
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			h.Components = append(h.Components, ComponentInfo{
				Name: d.Name, Kind: "Deployment", Namespace: "kube-system",
				Desired: desired, Ready: d.Status.ReadyReplicas,
				Images: containerImages(d.Spec.Template.Spec.Containers),
			})
			if d.Name == "coredns" {
				h.CoreDNS = CoreDNSInfo{Desired: desired, Ready: d.Status.ReadyReplicas}
			}
		}
	}

	if daemonsetsErr != nil {
		errs = append(errs, *daemonsetsErr)
	} else {
		for i := range daemonsets.Items {
			ds := &daemonsets.Items[i]
			h.Components = append(h.Components, ComponentInfo{
				Name: ds.Name, Kind: "DaemonSet", Namespace: "kube-system",
				Desired: ds.Status.DesiredNumberScheduled, Ready: ds.Status.NumberReady,
				Images: containerImages(ds.Spec.Template.Spec.Containers),
			})
		}
	}

	// Deployment first, then DaemonSet, both by name: deterministic output.
	sortComponents(h.Components)
	return h, errs
}

func containerImages(containers []corev1.Container) []string {
	var imgs []string
	seen := map[string]bool{}
	for _, c := range containers {
		if c.Image != "" && !seen[c.Image] {
			seen[c.Image] = true
			imgs = append(imgs, c.Image)
		}
	}
	return imgs
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// readyzSummary keeps only the failed ([-]) checks by default so a healthy
// control plane produces an empty field instead of 36 lines of noise; --full
// returns the raw verbose body. A non-verbose body (no check lines) is kept
// as-is because it carries the bare failure message.
func readyzSummary(body string, full bool) string {
	raw := strings.TrimSpace(body)
	if full {
		return raw
	}
	var failed []string
	hasChecks := false
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "[-]"):
			failed = append(failed, line)
			hasChecks = true
		case strings.HasPrefix(line, "[+]"):
			hasChecks = true
		}
	}
	if len(failed) > 0 {
		return strings.Join(failed, "\n")
	}
	if hasChecks {
		return ""
	}
	return raw
}

func sortComponents(cs []ComponentInfo) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Kind != cs[j].Kind {
			return cs[i].Kind < cs[j].Kind
		}
		return cs[i].Name < cs[j].Name
	})
}
