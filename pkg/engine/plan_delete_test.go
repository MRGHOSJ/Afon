package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

// TestWorstOp_RiskOrder pins the multi-object status order: delete is
// destructive and must never hide behind a create or update (review
// order delete > create > update > none; apply executes deletes last —
// the inversion is deliberate and documented in design.md).
func TestWorstOp_RiskOrder(t *testing.T) {
	d := func(op driver.Op) driver.Diff { return driver.Diff{Op: op} }
	cases := []struct {
		name  string
		diffs []driver.Diff
		want  string
	}{
		{"delete beats create", []driver.Diff{d(driver.OpCreate), d(driver.OpDelete)}, PlanDelete},
		{"delete beats everything", []driver.Diff{d(driver.OpUpdate), d(driver.OpDelete), d(driver.OpCreate)}, PlanDelete},
		{"create beats update", []driver.Diff{d(driver.OpUpdate), d(driver.OpCreate)}, PlanCreate},
		{"create beats none", []driver.Diff{d(driver.OpNone), d(driver.OpCreate)}, PlanCreate},
		{"update beats none", []driver.Diff{d(driver.OpNone), d(driver.OpUpdate)}, PlanUpdate},
		{"all none", []driver.Diff{d(driver.OpNone), d(driver.OpNone)}, PlanNone},
		{"single delete", []driver.Diff{d(driver.OpDelete)}, PlanDelete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := worstOp(tc.diffs); got != tc.want {
				t.Errorf("worstOp() = %q, want %q", got, tc.want)
			}
		})
	}
}

// deleteStub answers every desired object with a delete diff, standing in
// for an inventory driver pruning its own objects.
type deleteStub struct{}

func (deleteStub) Plan(ctx context.Context, owner driver.Owner, desired []driver.Object) ([]driver.Diff, error) {
	diffs := make([]driver.Diff, len(desired))
	for i, o := range desired {
		diffs[i] = driver.Diff{Object: o, Op: driver.OpDelete}
	}
	return diffs, nil
}

func (deleteStub) Apply(ctx context.Context, owner driver.Owner, diffs []driver.Diff) error {
	return nil
}

const singleObjectFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: obj
      type: fake-object
      config:
        kind: ConfigMap
        name: hello
`

// TestPlan_DeleteStatus: a driver-reported delete plans as status
// "delete", renders on the plan line, and applies with a "deleted"
// detail.
func TestPlan_DeleteStatus(t *testing.T) {
	cf, err := compile(t, singleObjectFlow)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	p, err := BuildPlan(ctx, cf, NewMemoryStore(), deleteStub{})
	if err != nil {
		t.Fatal(err)
	}
	e := p.Entries[0]
	if e.Status != PlanDelete {
		t.Errorf("status = %q, want %q", e.Status, PlanDelete)
	}
	if e.Diff == nil || e.Diff.Op != driver.OpDelete {
		t.Errorf("diff = %+v, want a delete diff", e.Diff)
	}
	text := p.RenderText()
	if !strings.Contains(text, PlanDelete) {
		t.Errorf("render must show delete status:\n%s", text)
	}
	if !strings.Contains(text, "1 delete entry") {
		t.Errorf("summary must count the delete as an entry:\n%s", text)
	}

	// Apply path: the changed diff formats as "deleted Kind/Name".
	res, err := Apply(ctx, cf, NewMemoryStore(), deleteStub{}, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := res.Get("obj"); r.Detail != "deleted ConfigMap/hello" {
		t.Errorf("detail = %q, want %q", r.Detail, "deleted ConfigMap/hello")
	}
}

// TestPlan_SummaryDeleteFirst: the summary line lists deletes before
// creates so a destructive plan is visible at a glance.
func TestPlan_SummaryDeleteFirst(t *testing.T) {
	p := &Plan{
		Flow:    "x",
		Summary: map[string]int{PlanCreate: 1, PlanDelete: 2, PlanUpdate: 3},
	}
	out := p.RenderText()
	iDel := strings.Index(out, "2 delete entry")
	iUpd := strings.Index(out, "3 update")
	iCre := strings.Index(out, "1 create")
	if iDel < 0 || iCre < 0 || iUpd < 0 {
		t.Fatalf("summary missing entries:\n%s", out)
	}
	if !(iDel < iCre && iCre < iUpd) {
		t.Errorf("summary order must be delete, create, update:\n%s", out)
	}
}

// TestPlan_DeleteObjectCount: the prompt's count walks OpDelete diffs
// across every entry shape — single-diff, mixed batch, create-only,
// diff-less — and never reads Summary, whose entry count undercounts a
// batch where deletes share a node with creates (the demo's 4-object
// batch reported 1 entry).
func TestPlan_DeleteObjectCount(t *testing.T) {
	od := func(op driver.Op, name string) driver.Diff {
		return driver.Diff{Op: op, Object: driver.Object{Kind: "Service", Name: name}}
	}
	p := &Plan{
		Flow: "x",
		Entries: []PlanEntry{
			{NodeID: "one", Kind: KindResource, Status: PlanDelete,
				Diff: &driver.Diff{Op: driver.OpDelete, Object: driver.Object{Kind: "Service", Name: "solo"}}},
			{NodeID: "batch", Kind: KindResource, Status: PlanDelete, Diffs: []driver.Diff{
				od(driver.OpCreate, "new-a"),
				od(driver.OpDelete, "old-a"),
				od(driver.OpDelete, "old-b"),
				od(driver.OpUpdate, "mid"),
			}},
			{NodeID: "fresh", Kind: KindResource, Status: PlanCreate,
				Diff: &driver.Diff{Op: driver.OpCreate, Object: driver.Object{Kind: "ConfigMap", Name: "hello"}}},
			{NodeID: "act", Kind: KindAction, Status: PlanCached, Key: "abcdef1234567890"},
		},
		// Entry-count summary: 2 delete entries, 1 create — must NOT
		// be what the count reads.
		Summary: map[string]int{PlanDelete: 2, PlanCreate: 1},
	}
	if got, want := p.DeleteObjectCount(), 3; got != want {
		t.Errorf("DeleteObjectCount() = %d, want %d", got, want)
	}
	if got := (&Plan{}).DeleteObjectCount(); got != 0 {
		t.Errorf("empty plan DeleteObjectCount() = %d, want 0", got)
	}
}
