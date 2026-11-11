// Flow-pipeline e2e: validate → plan → apply against the fake driver,
// across separate processes. The cross-process part is the point: the
// cache and fake state must survive between invocations for a no-op rerun
// to show cached/none.
package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const e2eFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: e2e
spec:
  nodes:
    - id: image
      type: image-ref
      config:
        ref: docker.io/library/nginx:1.27
    - id: obj
      type: fake-object
      config:
        kind: ConfigMap
        name: hello
        namespace: default
        data:
          digest: ${image.digest}
`

func writeFlow(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "flow.yaml")
	if err := os.WriteFile(path, []byte(e2eFlow), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFlow_Validate(t *testing.T) {
	path := writeFlow(t)
	r := runAf(t, nil, "validate", "-f", path)
	if r.code != 0 {
		t.Fatalf("validate exit = %d\nstderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, `flow "e2e" is valid`) {
		t.Errorf("stdout = %q", r.stdout)
	}

	// A broken flow fails with a position.
	bad := filepath.Join(filepath.Dir(path), "bad.yaml")
	src := strings.Replace(e2eFlow, "type: image-ref", "type: no-such", 1)
	if err := os.WriteFile(bad, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	r = runAf(t, nil, "validate", "-f", bad)
	if r.code != 1 {
		t.Fatalf("validate of broken flow exit = %d, want 1", r.code)
	}
	if !strings.Contains(r.stderr, "bad.yaml:") {
		t.Errorf("error must carry a position, stderr = %q", r.stderr)
	}
	if !strings.Contains(r.stderr, "unknown node type") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

// TestFlow_ApplyTwiceThenPlan is the headline e2e: two applies (separate
// processes), then a plan showing only cached and none. Also asserts the
// fake driver banner and that state lives next to the flow file, not cwd.
func TestFlow_ApplyTwiceThenPlan(t *testing.T) {
	path := writeFlow(t)
	stateDir := filepath.Dir(path)

	// Run from a different cwd to prove state follows the flow file.
	otherDir := t.TempDir()

	run := func(args ...string) result {
		t.Helper()
		// runAf inherits the test's cwd; temporarily switch it.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(otherDir); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chdir(wd); err != nil {
				t.Fatal(err)
			}
		}()
		return runAf(t, nil, args...)
	}

	// Plan before anything: will-run + known-after-apply.
	r := run("plan", "-f", path)
	if r.code != 0 {
		t.Fatalf("plan exit = %d\nstderr: %s", r.code, r.stderr)
	}
	// Banner goes to stderr so -o json stays parseable.
	if !strings.Contains(r.stderr, "fake driver: no cluster is touched") {
		t.Errorf("missing fake-driver banner on stderr:\n%s", r.stderr)
	}
	if !strings.Contains(r.stdout, "will-run") {
		t.Errorf("first plan should predict will-run:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "known-after-apply") {
		t.Errorf("first plan should predict known-after-apply:\n%s", r.stdout)
	}
	// State dir is the flow's dir, not the cwd we ran from: nothing may
	// appear under otherDir.
	if _, err := os.Stat(filepath.Join(otherDir, ".afon")); !os.IsNotExist(err) {
		t.Errorf("plan must not create state in cwd; .afon err = %v", err)
	}

	// Apply 1: everything runs. Parallelism 1 keeps node output order
	// deterministic for the detail assertions below.
	r = run("apply", "-f", path, "--parallelism", "1")
	if r.code != 0 {
		t.Fatalf("apply 1 exit = %d\nstderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "succeeded") {
		t.Errorf("apply 1 should succeed:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "(created ConfigMap/hello)") {
		t.Errorf("apply 1 must report the created object detail:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "(cached)") {
		t.Errorf("apply 1 must not report cached:\n%s", r.stdout)
	}
	// State landed next to the flow file.
	if _, err := os.Stat(filepath.Join(stateDir, ".afon", "fake-state.json")); err != nil {
		t.Errorf("fake state missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, ".afon", "cache")); err != nil {
		t.Errorf("cache dir missing: %v", err)
	}

	// Apply 2 (fresh process): action cached, resource no diffs.
	r = run("apply", "-f", path, "--parallelism", "1")
	if r.code != 0 {
		t.Fatalf("apply 2 exit = %d\nstderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "image") || !strings.Contains(r.stdout, "(cached)") {
		t.Errorf("apply 2 must cache the action:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "(no changes)") {
		t.Errorf("apply 2 must report the resource's no-changes detail:\n%s", r.stdout)
	}
	// The resource node reports succeeded with no applied diffs; the plan
	// section above it must say none.
	if !strings.Contains(r.stdout, "none") {
		t.Errorf("apply 2 plan section should show none:\n%s", r.stdout)
	}

	// Plan after two applies: everything cached or none, nothing else.
	r = run("plan", "-f", path)
	if r.code != 0 {
		t.Fatalf("plan exit = %d\nstderr: %s", r.code, r.stderr)
	}
	for _, bad := range []string{"will-run", "create", "update", "known-after-apply"} {
		if strings.Contains(r.stdout, bad) {
			t.Errorf("plan after two applies still says %q:\n%s", bad, r.stdout)
		}
	}
	if !strings.Contains(r.stdout, "cached") || !strings.Contains(r.stdout, "none") {
		t.Errorf("plan should show cached and none:\n%s", r.stdout)
	}
}

// TestFlow_ValidateRejectsRollback: rollback fails at validate time, not
// halfway through a run.
func TestFlow_ValidateRejectsRollback(t *testing.T) {
	path := writeFlow(t)
	src := strings.Replace(e2eFlow, "type: image-ref", "type: image-ref\n      onFailure: rollback", 1)
	broken := filepath.Join(filepath.Dir(path), "rollback.yaml")
	if err := os.WriteFile(broken, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"validate", "plan", "apply"} {
		r := runAf(t, nil, cmd, "-f", broken)
		if r.code != 1 {
			t.Errorf("%s exit = %d, want 1", cmd, r.code)
		}
		combined := r.stdout + r.stderr
		if !strings.Contains(combined, "not implemented until Phase 4") {
			t.Errorf("%s must reject rollback at validate time:\n%s", cmd, combined)
		}
	}
}

// TestFlow_UnknownDriver: --driver is the only selection mechanism and
// says what exists.
func TestFlow_UnknownDriver(t *testing.T) {
	path := writeFlow(t)
	r := runAf(t, nil, "plan", "-f", path, "--driver", "k8s")
	if r.code != 1 {
		t.Fatalf("exit = %d, want 1", r.code)
	}
	wantStderrContains(t, r, `unknown driver "k8s"`, "fake")
}

// TestFlow_JSONOutput pins both directions of the -o json contract: stdout
// must parse as JSON (banner and errors never leak into it) and the
// fake-driver banner must appear on stderr. A regression in either
// direction fails here.
func TestFlow_JSONOutput(t *testing.T) {
	path := writeFlow(t)
	const banner = "fake driver: no cluster is touched"

	// plan -o json: a Plan object with entries.
	r := runAf(t, nil, "plan", "-f", path, "-o", "json")
	if r.code != 0 {
		t.Fatalf("plan exit = %d\nstderr: %s", r.code, r.stderr)
	}
	var plan struct {
		Flow    string `json:"flow"`
		Entries []struct {
			NodeID string `json:"nodeId"`
			Status string `json:"status"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &plan); err != nil {
		t.Fatalf("plan stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if plan.Flow != "e2e" || len(plan.Entries) != 2 {
		t.Errorf("plan JSON = %+v, want flow e2e with 2 entries", plan)
	}
	if !strings.Contains(r.stderr, banner) {
		t.Errorf("plan stderr missing banner:\n%s", r.stderr)
	}
	if strings.Contains(r.stdout, banner) {
		t.Errorf("banner leaked into plan stdout:\n%s", r.stdout)
	}

	// apply -o json: a snapshot keyed by node id.
	r = runAf(t, nil, "apply", "-f", path, "-o", "json")
	if r.code != 0 {
		t.Fatalf("apply exit = %d\nstderr: %s", r.code, r.stderr)
	}
	var snap map[string]struct {
		NodeID string `json:"nodeId"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &snap); err != nil {
		t.Fatalf("apply stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if _, ok := snap["image"]; !ok {
		t.Errorf("apply JSON missing image node: %s", r.stdout)
	}
	if _, ok := snap["obj"]; !ok {
		t.Errorf("apply JSON missing obj node: %s", r.stdout)
	}
	if !strings.Contains(r.stderr, banner) {
		t.Errorf("apply stderr missing banner:\n%s", r.stderr)
	}
	if strings.Contains(r.stdout, banner) {
		t.Errorf("banner leaked into apply stdout:\n%s", r.stdout)
	}
}

// TestFlow_ResetState: deleting .afon/ is the reset — the next plan must
// predict everything from scratch (will-run + known-after-apply) and the
// next apply must recreate the objects, not report cached/none.
func TestFlow_ResetState(t *testing.T) {
	path := writeFlow(t)
	stateDir := filepath.Dir(path)

	// Warm state: apply, then confirm the plan is settled.
	if r := runAf(t, nil, "apply", "-f", path); r.code != 0 {
		t.Fatalf("apply exit = %d\nstderr: %s", r.code, r.stderr)
	}
	r := runAf(t, nil, "plan", "-f", path)
	if r.code != 0 {
		t.Fatalf("warm plan exit = %d\nstderr: %s", r.code, r.stderr)
	}
	for _, bad := range []string{"will-run", "known-after-apply"} {
		if strings.Contains(r.stdout, bad) {
			t.Errorf("warm plan still says %q:\n%s", bad, r.stdout)
		}
	}

	// Reset: wipe the state directory contents.
	if err := os.RemoveAll(filepath.Join(stateDir, ".afon")); err != nil {
		t.Fatal(err)
	}

	// Cold plan: everything predicted again.
	r = runAf(t, nil, "plan", "-f", path)
	if r.code != 0 {
		t.Fatalf("cold plan exit = %d\nstderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "will-run") {
		t.Errorf("plan after reset should predict will-run:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "known-after-apply") {
		t.Errorf("plan after reset should predict known-after-apply:\n%s", r.stdout)
	}

	// Cold apply: the object is created again, not reported as unchanged.
	r = runAf(t, nil, "apply", "-f", path)
	if r.code != 0 {
		t.Fatalf("cold apply exit = %d\nstderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "(created ConfigMap/hello)") {
		t.Errorf("apply after reset must recreate the object:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "(no changes)") {
		t.Errorf("apply after reset must not report no changes:\n%s", r.stdout)
	}
}
