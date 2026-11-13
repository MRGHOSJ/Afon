package main

import (
	"os"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/engine"
	"github.com/MRGHOSJ/afon/pkg/schema"
)

// renderFixture covers every apply-result suffix kind: cached, detail
// (created and no-changes), error, cancelled, plus one bare success with
// no suffix at all (the line must be trimmed, not padded into space).
func renderFixture() *engine.RunResult {
	return &engine.RunResult{Nodes: map[string]engine.NodeResult{
		"act":    {NodeID: "act", Status: engine.StatusSucceeded},
		"image":  {NodeID: "image", Status: engine.StatusSucceeded, Cached: true},
		"obj":    {NodeID: "obj", Status: engine.StatusSucceeded, Detail: "created ConfigMap/hello"},
		"obj2":   {NodeID: "obj2", Status: engine.StatusSucceeded, Detail: "no changes"},
		"slow":   {NodeID: "slow", Status: engine.StatusCancelled, ErrText: "context canceled"},
		"worker": {NodeID: "worker", Status: engine.StatusFailed, ErrText: "boom"},
	}}
}

// TestRenderRunResult_Golden pins the exact layout: status column padded
// to the longest status, one space, then a non-space-leading suffix.
func TestRenderRunResult_Golden(t *testing.T) {
	// Columns: two leading spaces + 16-char id field + one space = status
	// at index 19; statuses pad to the longest ("succeeded"/"cancelled",
	// 9 chars); one space separates the status field from the suffix.
	want := "apply result:" +
		"\n  act              succeeded" + // id 3 → 13 pad + 1 sep
		"\n  image            succeeded (cached)" + // id 5 → 11 pad + 1 sep
		"\n  obj              succeeded (created ConfigMap/hello)" +
		"\n  obj2             succeeded (no changes)" + // id 4 → 12 pad + 1 sep
		"\n  slow             cancelled : context canceled" +
		"\n  worker           failed    : boom" + // id 6 → 10 pad; failed → 3 pad + 1 sep
		"\n"
	if got := renderRunResult(renderFixture()); got != want {
		t.Errorf("renderRunResult() mismatch\ngot:\n%q\nwant:\n%q", got, want)
	}
}

// TestRenderRunResult_DetailAndPriority asserts the suffix contents and
// the priority rule: error wins over cached and detail.
func TestRenderRunResult_DetailAndPriority(t *testing.T) {
	out := renderRunResult(renderFixture())
	for _, want := range []string{
		"(created ConfigMap/hello)",
		"(no changes)",
		"(cached)",
		": boom",
		": context canceled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Priority: the failed node has no detail or cached marker layered on
	// top of its error, and no line shows two suffixes at once.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "(cached)") && strings.Contains(line, ":") {
			t.Errorf("line mixes cached and error suffixes: %q", line)
		}
		if strings.Contains(line, "(created") && strings.Contains(line, ":") {
			t.Errorf("line mixes detail and error suffixes: %q", line)
		}
	}
}

// suffixColumn finds where a rendered line's suffix begins: locate the
// status field, skip the spaces after the status token. Returns -1 when
// the line carries no suffix (trailing spaces were trimmed).
func suffixColumn(line string, statusStart int) int {
	if len(line) <= statusStart {
		return -1
	}
	rest := line[statusStart:]
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		return -1
	}
	for end < len(rest) && rest[end] == ' ' {
		end++
	}
	if end == len(rest) {
		return -1
	}
	return statusStart + end
}

// TestRenderRunResult_Alignment: every line with a suffix must start that
// suffix at the same column — find the status, skip the spaces, land on
// the suffix. Sturdier than exact strings against width changes.
func TestRenderRunResult_Alignment(t *testing.T) {
	// Layout: two leading spaces, 16-char id field, one space → status.
	const statusStart = 2 + 16 + 1
	out := renderRunResult(renderFixture())
	wantCol := -1
	seen := 0
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue // "apply result:" header
		}
		col := suffixColumn(line, statusStart)
		if col < 0 {
			continue // no suffix on this line
		}
		seen++
		if wantCol < 0 {
			wantCol = col
			continue
		}
		if col != wantCol {
			t.Errorf("suffix column %d, want %d on line %q", col, wantCol, line)
		}
	}
	if seen < 5 {
		t.Errorf("expected at least 5 lines with suffixes, got %d", seen)
	}
}

// TestPrintPlanHeader pins the header contract: fake plans start with
// just the driver name; k8s plans carry context, namespace, and the
// force/prune flag state. The header is text-mode only — callers gate
// it on flagOutput, and this function always prints.
func TestPrintPlanHeader(t *testing.T) {
	cf := &engine.CompiledFlow{Target: schema.Target{Namespace: "demo"}}

	capture := func(t *testing.T, driverName, ctxName string) string {
		t.Helper()
		stdout, stderr, err := captureStdoutStderr(t, func() error {
			printPlanHeader(driverName, ctxName, cf)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if stderr != "" {
			t.Errorf("header wrote to stderr: %q", stderr)
		}
		return stdout
	}

	if got := capture(t, "fake", ""); got != "driver: fake\n" {
		t.Errorf("fake header = %q, want %q", got, "driver: fake\n")
	}

	// k8s defaults: --force-conflicts off, prune on.
	prevForce, prevPrune := flagForceConflicts, flagNoPrune
	t.Cleanup(func() { flagForceConflicts, flagNoPrune = prevForce, prevPrune })

	flagForceConflicts, flagNoPrune = false, false
	want := "driver: k8s context: k3d-dev namespace: demo force: off prune: on\n"
	if got := capture(t, "k8s", "k3d-dev"); got != want {
		t.Errorf("k8s header = %q, want %q", got, want)
	}

	flagForceConflicts, flagNoPrune = true, true
	want = "driver: k8s context: prod namespace: demo force: on prune: off\n"
	if got := capture(t, "k8s", "prod"); got != want {
		t.Errorf("k8s header (force, no-prune) = %q, want %q", got, want)
	}
}

// deletePlan builds a plan whose entries carry real diffs totalling n
// OpDelete objects — the source DeleteObjectCount reads. The batch is
// deliberately mixed (n deletes + 2 creates in one entry, the demo's
// shape): a count of all diffs would say n+2 and fail these assertions,
// pinning that creates never inflate the delete count.
func deletePlan(n int) *engine.Plan {
	diffs := make([]driver.Diff, 0, n+2)
	for i := 0; i < n; i++ {
		diffs = append(diffs, driver.Diff{
			Op:     driver.OpDelete,
			Object: driver.Object{Kind: "Service", Name: "old-" + string(rune('a'+i))},
		})
	}
	for i := 0; i < 2; i++ {
		diffs = append(diffs, driver.Diff{
			Op:     driver.OpCreate,
			Object: driver.Object{Kind: "Deployment", Name: "new-" + string(rune('a'+i))},
		})
	}
	status := engine.PlanCreate
	if n > 0 {
		status = engine.PlanDelete
	}
	return &engine.Plan{Entries: []engine.PlanEntry{{
		NodeID: "deploy",
		Kind:   engine.KindResource,
		Status: status,
		Diffs:  diffs,
	}}}
}

// withStdin points os.Stdin at a temp file holding content (readable
// once, like a pipe answered and closed).
func withStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	prev := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = prev
		f.Close()
	})
}

func withTTY(t *testing.T, isTTY bool) {
	t.Helper()
	prev := stdinIsTerminal
	stdinIsTerminal = func() bool { return isTTY }
	t.Cleanup(func() { stdinIsTerminal = prev })
}

func withAutoApprove(t *testing.T, v bool) {
	t.Helper()
	prev := flagAutoApprove
	flagAutoApprove = v
	t.Cleanup(func() { flagAutoApprove = prev })
}

// TestConfirmDeletes pins the prompt contract: no deletes → no
// interaction; deletes on a non-TTY refuse naming --auto-approve;
// --auto-approve skips the prompt; a TTY prompt (stderr, stdout clean)
// proceeds only on y/yes, everything else aborts.
func TestConfirmDeletes(t *testing.T) {
	t.Run("no deletes proceeds without interaction", func(t *testing.T) {
		withTTY(t, false)
		withAutoApprove(t, false)
		withStdin(t, "")
		stdout, stderr, err := captureStdoutStderr(t, func() error { return confirmDeletes(deletePlan(0)) })
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("no-delete plan interacted: stdout=%q stderr=%q", stdout, stderr)
		}
	})

	t.Run("non-tty with deletes refuses", func(t *testing.T) {
		withTTY(t, false)
		withAutoApprove(t, false)
		stdout, stderr, err := captureStdoutStderr(t, func() error { return confirmDeletes(deletePlan(2)) })
		if err == nil || !strings.Contains(err.Error(), "--auto-approve") || !strings.Contains(err.Error(), "not interactive") {
			t.Errorf("err = %v, want non-interactive refusal naming --auto-approve", err)
		}
		if !strings.Contains(err.Error(), "2 delete(s)") {
			t.Errorf("err = %v, want the delete count", err)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("refusal wrote output: stdout=%q stderr=%q", stdout, stderr)
		}
	})

	t.Run("auto-approve skips the prompt", func(t *testing.T) {
		withTTY(t, false)
		withAutoApprove(t, true)
		stdout, stderr, err := captureStdoutStderr(t, func() error { return confirmDeletes(deletePlan(1)) })
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("auto-approve interacted: stdout=%q stderr=%q", stdout, stderr)
		}
	})

	yes := func(t *testing.T, answer string) {
		t.Helper()
		withTTY(t, true)
		withAutoApprove(t, false)
		withStdin(t, answer)
		stdout, stderr, err := captureStdoutStderr(t, func() error { return confirmDeletes(deletePlan(3)) })
		if err != nil {
			t.Fatalf("answer %q: err = %v, want nil", answer, err)
		}
		if stdout != "" {
			t.Errorf("answer %q: prompt leaked to stdout: %q", answer, stdout)
		}
		if !strings.Contains(stderr, "plan deletes 3 object(s); apply these changes? [y/N] ") {
			t.Errorf("answer %q: stderr missing prompt: %q", answer, stderr)
		}
	}

	t.Run("tty y proceeds", func(t *testing.T) { yes(t, "y\n") })
	t.Run("tty Y proceeds", func(t *testing.T) { yes(t, "Y\n") })
	t.Run("tty yes proceeds", func(t *testing.T) { yes(t, "yes\n") })

	no := func(t *testing.T, answer string) {
		t.Helper()
		withTTY(t, true)
		withAutoApprove(t, false)
		withStdin(t, answer)
		_, stderr, err := captureStdoutStderr(t, func() error { return confirmDeletes(deletePlan(1)) })
		if err == nil || !strings.Contains(err.Error(), "apply aborted") {
			t.Errorf("answer %q: err = %v, want apply aborted", answer, err)
		}
		if !strings.Contains(stderr, "apply these changes? [y/N]") {
			t.Errorf("answer %q: stderr missing prompt: %q", answer, stderr)
		}
	}

	t.Run("tty n aborts", func(t *testing.T) { no(t, "n\n") })
	t.Run("tty empty aborts", func(t *testing.T) { no(t, "") })
	t.Run("tty junk aborts", func(t *testing.T) { no(t, "sure\n") })
}
