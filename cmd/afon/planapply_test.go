package main

import (
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/engine"
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
