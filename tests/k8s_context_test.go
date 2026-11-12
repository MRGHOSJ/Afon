// k8s driver context resolution, end to end through the binary against
// a two-context kubeconfig. Both contexts point at a dead port so an
// accidental connection refuses instantly instead of hanging; no test
// here needs a cluster.
package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const twoContextKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: ca
  cluster:
    server: https://127.0.0.1:1
- name: cb
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: alpha
  context: {cluster: ca, user: u1}
- name: beta
  context: {cluster: cb, user: u1}
current-context: alpha
users:
- name: u1
  user: {}
`

// writeKctxFlow writes a flow whose target carries the given context
// fragment (e.g. ", context: beta" or "").
func writeKctxFlow(t *testing.T, targetFragment string) string {
	t.Helper()
	src := fmt.Sprintf(`apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: kctx
spec:
  target: {namespace: hello%s}
  nodes:
    - id: obj
      type: fake-object
      config: {kind: ConfigMap, name: x}
`, targetFragment)
	path := filepath.Join(t.TempDir(), "flow.yaml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTwoContextKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(twoContextKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestK8sContextDisagreement: --context and target.context must agree;
// a mismatch fails before any connection and prints both ways out.
func TestK8sContextDisagreement(t *testing.T) {
	flow := writeKctxFlow(t, ", context: beta")
	kc := writeTwoContextKubeconfig(t)
	r := runAf(t, map[string]string{"KUBECONFIG": kc},
		"plan", "-f", flow, "--driver", "k8s", "--context", "alpha")
	wantExit(t, r, "plan")
	wantStderrContains(t, r, "disagrees", `"alpha"`, `"beta"`, "--context", "target.context")
	if strings.Contains(r.stderr, "k8s driver:") {
		t.Errorf("banner must not print when resolution fails:\n%s", r.stderr)
	}
}

// TestK8sContextFromFlow: with only the flow's target.context set, the
// banner confirms it is the context actually used, then the connection
// to the dead port fails.
func TestK8sContextFromFlow(t *testing.T) {
	flow := writeKctxFlow(t, ", context: beta")
	kc := writeTwoContextKubeconfig(t)
	r := runAf(t, map[string]string{"KUBECONFIG": kc},
		"plan", "-f", flow, "--driver", "k8s")
	wantExit(t, r, "plan")
	wantStderrContains(t, r, "k8s driver: context=beta namespace=hello")
}

// TestK8sContextMissing: neither --context nor target.context is an
// error naming both ways out — the current context is never used
// implicitly.
func TestK8sContextMissing(t *testing.T) {
	flow := writeKctxFlow(t, "")
	kc := writeTwoContextKubeconfig(t)
	r := runAf(t, map[string]string{"KUBECONFIG": kc},
		"plan", "-f", flow, "--driver", "k8s")
	wantExit(t, r, "plan")
	wantStderrContains(t, r, "--context", "spec.target.context")
}
