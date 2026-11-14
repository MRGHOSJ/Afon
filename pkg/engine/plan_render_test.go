package engine

import (
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

// renderTextFixture is a hand-built plan whose entries all carry a
// suffix (key, diff, or reason), so every line exercises the status
// padding: cached, will-run, create, update, known-after-apply, none.
func renderTextFixture() *Plan {
	return &Plan{
		Flow: "t",
		File: "flow.yaml",
		Entries: []PlanEntry{
			{NodeID: "act", Kind: KindAction, Status: PlanCached, Key: "abcdef1234567890"},
			{NodeID: "cold", Kind: KindAction, Status: PlanWillRun, Key: "abcdef1234567890"},
			{NodeID: "mk", Kind: KindResource, Status: PlanCreate,
				Diff: &driver.Diff{Op: driver.OpCreate, Object: driver.Object{Kind: "ConfigMap", Name: "hello"}}},
			{NodeID: "web", Kind: KindResource, Status: PlanUpdate,
				Diff: &driver.Diff{Op: driver.OpUpdate, Object: driver.Object{Kind: "Deployment", Name: "web"}}},
			{NodeID: "poison", Kind: KindResource, Status: PlanKnownAfterApply,
				Reason: "upstream \"act\" did not run"},
			{NodeID: "batch", Kind: KindResource, Status: PlanDelete, Diffs: []driver.Diff{
				{Op: driver.OpCreate, Object: driver.Object{Kind: "Deployment", Name: "new"}},
				{Op: driver.OpCreate, Object: driver.Object{Kind: "Service", Name: "new"}},
				{Op: driver.OpDelete, Object: driver.Object{Kind: "Deployment", Name: "old"}},
				{Op: driver.OpDelete, Object: driver.Object{Kind: "Service", Name: "old"}},
			}},
			{NodeID: "same", Kind: KindResource, Status: PlanNone,
				Reason: "already matches"},
		},
		Summary: map[string]int{},
	}
}

// planSuffixColumn finds where a plan line's suffix begins: locate the
// status field (after the id and kind fields), skip the spaces after the
// status token. Returns -1 when the line carries no suffix.
func planSuffixColumn(line string, statusStart int) int {
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

// TestRenderText_Alignment: every plan line with a suffix must start that
// suffix at the same column — find the status, skip the spaces, land on
// the suffix. Statuses differ in length (none vs known-after-apply), so
// this fails if the status column is not padded.
func TestRenderText_Alignment(t *testing.T) {
	// Layout: two leading spaces, 16-char id field, one space, 8-char kind
	// field, one space → status.
	const statusStart = 2 + 16 + 1 + 8 + 1
	out := renderTextFixture().RenderText()
	wantCol := -1
	seen := 0
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue // header and summary lines
		}
		col := planSuffixColumn(line, statusStart)
		if col < 0 {
			t.Errorf("line has no suffix: %q", line)
			continue
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
	if seen != len(renderTextFixture().Entries) {
		t.Errorf("lines with suffixes = %d, want %d", seen, len(renderTextFixture().Entries))
	}
}

// TestRenderText_Suffixes pins the suffix contents that ride after the
// padded status column.
func TestRenderText_Suffixes(t *testing.T) {
	out := renderTextFixture().RenderText()
	for _, want := range []string{
		"key=abcdef12345",
		"ConfigMap hello",
		"Deployment web",
		"4 changes",
		`upstream "act" did not run`,
		"already matches",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "4 objects") {
		t.Errorf("multi-diff suffix must read \"changes\", not \"objects\":\n%s", out)
	}
}

// TestRenderText_ChangeCountSkipsNone: the suffix counts changes, not
// batch members. An OpNone diff (checked, unchanged) must not inflate
// the count — the demo shape [none, delete] reads "1 change" (singular:
// planResource only ever set Diffs with len > 1, so N=1 is reachable
// only once none is excluded), and a mixed batch counts only its real
// changes. The absence assertions matter: "1 change" is a substring of
// "1 changes", so Contains alone would pass a pluralization regression.
func TestRenderText_ChangeCountSkipsNone(t *testing.T) {
	cases := []struct {
		name  string
		diffs []driver.Diff
		want  []string
		not   []string
	}{
		{
			name: "one none plus one delete is one change",
			diffs: []driver.Diff{
				{Op: driver.OpNone, Object: driver.Object{Kind: "ConfigMap", Name: "same"}},
				{Op: driver.OpDelete, Object: driver.Object{Kind: "Deployment", Name: "gone"}},
			},
			want: []string{"1 change"},
			not:  []string{"1 changes", "2 changes"},
		},
		{
			name: "none in a mixed batch does not inflate the count",
			diffs: []driver.Diff{
				{Op: driver.OpCreate, Object: driver.Object{Kind: "ConfigMap", Name: "new"}},
				{Op: driver.OpDelete, Object: driver.Object{Kind: "Deployment", Name: "old"}},
				{Op: driver.OpNone, Object: driver.Object{Kind: "Service", Name: "same"}},
			},
			want: []string{"2 changes"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plan{
				Flow: "t",
				Entries: []PlanEntry{{
					NodeID: "batch", Kind: KindResource,
					Status: worstOp(tc.diffs), Diffs: tc.diffs,
				}},
				Summary: map[string]int{},
			}
			out := p.RenderText()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(out, not) {
					t.Errorf("output must not contain %q:\n%s", not, out)
				}
			}
		})
	}
}
