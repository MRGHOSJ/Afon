package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/release"
)

// invLive builds a live object for an inventory name under the demo
// ConfigMap identity the record fixtures use.
func invLive(name string) driver.LiveObject {
	return driver.LiveObject{
		Object: driver.Object{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: name},
		UID:    "uid-" + name, Hash: "hash-" + name,
	}
}

// invEntry builds an inventory entry for that same identity.
func invEntry(node, name string) release.InventoryObject {
	return release.InventoryObject{
		Version: "v1", Kind: "ConfigMap", Namespace: "demo",
		Name: name, Node: node, UID: "uid-" + name, Hash: "hash-" + name,
	}
}

// cm is the desired form of that same identity: recorded inventory
// carries the served version, so desired objects must match it — or
// the prune would misread a still-desired object as an orphan.
func cm(name string) driver.Object {
	return driver.Object{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: name}
}

// promotedWithInventory returns a backend whose newest record carries
// the given inventory.
func promotedWithInventory(t *testing.T, inv ...release.InventoryObject) *memBackend {
	t.Helper()
	m := &memBackend{}
	if err := m.Promote(context.Background(), release.Record{
		Flow: "rec", Sequence: 1, Status: release.StatusSucceeded, Inventory: inv,
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestBuildPlanOptsInventoryPrune: with a Backend, each resource
// node's entry carries delete diffs for recorded objects that node no
// longer desires — attribution by recorded Node — and without a
// Backend (BuildPlan) nothing changes and no reads happen.
func TestBuildPlanOptsInventoryPrune(t *testing.T) {
	ops := map[string]driver.Op{"a": driver.OpNone, "d": driver.OpNone}
	flow, _ := recordFixture(t, ops)
	rd := newReaderDriver(ops, invLive("a"), invLive("d"), invLive("gone"), invLive("gone2"))
	backend := promotedWithInventory(t, invEntry("r1", "gone"), invEntry("r2", "gone2"))

	p, err := BuildPlanOpts(context.Background(), flow, NewMemoryStore(), rd,
		PlanOptions{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 2 {
		t.Fatalf("entries = %d", len(p.Entries))
	}
	r1, r2 := p.Entries[0], p.Entries[1]
	if r1.NodeID != "r1" || r2.NodeID != "r2" {
		t.Fatalf("entry order = %s, %s", r1.NodeID, r2.NodeID)
	}
	if len(r1.Diffs) != 2 || r1.Status != PlanDelete {
		t.Fatalf("r1 = status %q diffs %v", r1.Status, r1.Diffs)
	}
	if r1.Diffs[1].Object.Name != "gone" || r1.Diffs[1].Op != driver.OpDelete {
		t.Errorf("r1 inventory delete = %+v", r1.Diffs[1])
	}
	if len(r2.Diffs) != 2 || r2.Diffs[1].Object.Name != "gone2" || r2.Diffs[1].Op != driver.OpDelete {
		t.Errorf("r2 diffs = %+v", r2.Diffs)
	}
	// The demo's plan shape end to end: [none, delete] renders as one
	// change — pins how the inventory merge builds Diffs against the
	// suffix count (absence check: "1 change" is a substring of
	// "1 changes").
	if out := p.RenderText(); !strings.Contains(out, "1 change") ||
		strings.Contains(out, "1 changes") {
		t.Errorf("render must read \"1 change\":\n%s", out)
	}
	// BuildPlan (no opts) delegates with a zero PlanOptions: the
	// recorded objects never surface and the reader is never touched.
	rd.reads = 0
	p2, err := BuildPlan(context.Background(), flow, NewMemoryStore(), rd)
	if err != nil {
		t.Fatal(err)
	}
	if rd.reads != 0 {
		t.Errorf("BuildPlan read live state %d times", rd.reads)
	}
	if d := p2.Entries[0]; len(d.Diffs) != 0 || d.Diff == nil || d.Diff.Op != driver.OpNone || d.Status != PlanNone {
		t.Errorf("BuildPlan r1 = %+v", d)
	}
}

// TestInventoryPruneDedupesLabelPrune pins A6: when label pruning
// already produced a delete for an object the inventory also covers,
// exactly one delete survives — the inventory's live-identity version.
func TestInventoryPruneDedupesLabelPrune(t *testing.T) {
	o := driver.Object{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "x"}
	planned := []driver.Diff{{Object: o, Op: driver.OpDelete}}
	backend := promotedWithInventory(t, invEntry("r1", "x"))
	rd := newReaderDriver(nil, invLive("x"))

	out, err := inventoryPruneDiffs(context.Background(), backend, rd,
		driver.Owner{Flow: "rec", Node: "r1"},
		[]driver.Object{waitObj("a")}, planned)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("duplicates survived: %v", out)
	}
	if out[0].Op != driver.OpDelete || out[0].Object.Key() != o.Key() {
		t.Errorf("kept delete = %+v", out[0])
	}
}

// TestInventoryPruneSkipsAbsentAndDesired: a recorded object that is
// no longer live reads absent (already label-pruned, or deleted out of
// band) and is not re-planned; one that is still desired is skipped as
// desired, not pruned.
func TestInventoryPruneSkipsAbsentAndDesired(t *testing.T) {
	backend := promotedWithInventory(t, invEntry("r1", "gone"), invEntry("r1", "a"))
	rd := newReaderDriver(nil) // nothing is live
	planned := []driver.Diff{{Object: cm("a"), Op: driver.OpNone}}

	out, err := inventoryPruneDiffs(context.Background(), backend, rd,
		driver.Owner{Flow: "rec", Node: "r1"},
		[]driver.Object{cm("a")}, planned)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Op != planned[0].Op || out[0].Object.Key() != planned[0].Object.Key() {
		t.Errorf("out = %+v", out)
	}
	// "a" is desired (skipped as candidate), "gone" reads absent.
	if rd.reads != 1 {
		t.Errorf("reads = %d", rd.reads)
	}
}

// TestInventoryPruneOtherNodeIgnored: inventory for another node's
// objects never reaches this node's call — no candidates, no reads.
func TestInventoryPruneOtherNodeIgnored(t *testing.T) {
	backend := promotedWithInventory(t, invEntry("r9", "gone"))
	rd := newReaderDriver(nil, invLive("gone"))

	out, err := inventoryPruneDiffs(context.Background(), backend, rd,
		driver.Owner{Flow: "rec", Node: "r1"},
		[]driver.Object{waitObj("a")}, []driver.Diff{{Object: waitObj("a"), Op: driver.OpNone}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Errorf("out = %+v", out)
	}
	if rd.reads != 0 {
		t.Errorf("reads = %d, want 0", rd.reads)
	}
}

// TestInventoryPruneNoReaderSkips: a driver without ObjectReader
// verifies nothing, so inventory prune adds nothing and never fails.
func TestInventoryPruneNoReaderSkips(t *testing.T) {
	flow, d := recordFixture(t, nil)
	backend := promotedWithInventory(t, invEntry("r1", "gone"))
	planned := []driver.Diff{{Object: waitObj("a"), Op: driver.OpNone}}

	out, err := inventoryPruneDiffs(context.Background(), backend, d,
		driver.Owner{Flow: flow.Name, Node: "r1"},
		[]driver.Object{waitObj("a")}, planned)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Errorf("out = %+v", out)
	}
}

// listFailBackend fails only List — the read path inventory prune
// starts from.
type listFailBackend struct {
	release.Backend
	err error
}

func (b listFailBackend) List(ctx context.Context) ([]release.Record, error) {
	return nil, b.err
}

// TestBuildPlanOptsInventoryListError: a record-read failure is loud
// at plan time — the entry degrades to known-after-apply naming the
// cause instead of silently planning without pruning.
func TestBuildPlanOptsInventoryListError(t *testing.T) {
	ops := map[string]driver.Op{"a": driver.OpNone, "d": driver.OpNone}
	flow, _ := recordFixture(t, ops)
	rd := newReaderDriver(ops, invLive("a"), invLive("d"))
	backend := listFailBackend{Backend: &memBackend{}, err: errors.New("secrets unavailable")}

	p, err := BuildPlanOpts(context.Background(), flow, NewMemoryStore(), rd,
		PlanOptions{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	got := p.Entries[0]
	if got.Status != PlanKnownAfterApply {
		t.Fatalf("r1 status = %q", got.Status)
	}
	if !strings.Contains(got.Reason, "inventory prune: reading release records: secrets unavailable") {
		t.Errorf("reason = %q", got.Reason)
	}
}

// TestPruneInventorySkipsEmptyRecords: pending releases and failed
// runs that applied nothing carry no inventory; the newest record
// that does name objects still supplies it, so a broken apply in
// between does not silence pruning.
func TestPruneInventorySkipsEmptyRecords(t *testing.T) {
	m := &memBackend{}
	if err := m.Promote(context.Background(), release.Record{
		Sequence: 1, Status: release.StatusSucceeded,
		Inventory: []release.InventoryObject{invEntry("r1", "x")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Promote(context.Background(), release.Record{
		Sequence: 2, Status: release.StatusFailed,
	}); err != nil {
		t.Fatal(err)
	}
	inv, err := pruneInventory(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 1 || inv[0].Name != "x" {
		t.Errorf("inv = %+v", inv)
	}
	// Nothing recorded at all: nil inventory, no error.
	if inv, err := pruneInventory(context.Background(), &memBackend{}); err != nil || inv != nil {
		t.Errorf("empty backend = %v, %v", inv, err)
	}
}

// TestApplyInventoryPruneDeletesOrphan: apply executes the
// inventory-prune delete plan previews — the recorded object the flow
// no longer desires reaches the driver's Apply as an OpDelete under
// its recorded node, and only the owning node's batch carries it.
func TestApplyInventoryPruneDeletesOrphan(t *testing.T) {
	ops := map[string]driver.Op{"a": driver.OpNone, "d": driver.OpNone}
	flow, _ := recordFixture(t, ops)
	rd := newReaderDriver(ops, invLive("a"), invLive("d"), invLive("gone"))
	backend := promotedWithInventory(t, invEntry("r1", "gone"))

	res, err := Apply(context.Background(), flow, NewMemoryStore(), rd,
		ExecutorOptions{Backend: backend, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rd.applied) != 1 {
		t.Fatalf("apply batches = %d (%v)", len(rd.applied), rd.applied)
	}
	batch := rd.applied[0]
	if len(batch) != 1 || batch[0].Op != driver.OpDelete || batch[0].Object.Name != "gone" {
		t.Errorf("batch = %+v", batch)
	}
	for _, w := range res.Warnings() {
		t.Errorf("unexpected warning: %s", w)
	}
}

// TestApplyInventoryPruneDisabled: Prune off (CLI --no-prune) leaves
// the orphan alone — but the release record is still written.
func TestApplyInventoryPruneDisabled(t *testing.T) {
	ops := map[string]driver.Op{"a": driver.OpNone, "d": driver.OpNone}
	flow, _ := recordFixture(t, ops)
	rd := newReaderDriver(ops, invLive("a"), invLive("d"), invLive("gone"))
	backend := promotedWithInventory(t, invEntry("r1", "gone"))
	_, promotesBefore, _, _ := backend.counts()

	res, err := Apply(context.Background(), flow, NewMemoryStore(), rd,
		ExecutorOptions{Backend: backend, Prune: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(rd.applied) != 0 {
		t.Errorf("applied = %v, want none", rd.applied)
	}
	_, promotes, _, promoted := backend.counts()
	if promotes-promotesBefore != 1 || promoted != 2 {
		t.Errorf("apply promotes = %d, record written = %d", promotes-promotesBefore, promoted)
	}
	for _, w := range res.Warnings() {
		t.Errorf("unexpected warning: %s", w)
	}
}

// errReader fails every live read.
type errReader struct {
	*scriptedDriver
}

func (d *errReader) Read(ctx context.Context, owner driver.Owner, objs []driver.Object) ([]driver.LiveObject, error) {
	return nil, errors.New("boom")
}

// TestApplyInventoryPruneReadErrorWarns: record reads never fail an
// apply — a prune read error degrades to a warning and the run keeps
// going with label-prune only (ExecutorOptions contract).
func TestApplyInventoryPruneReadErrorWarns(t *testing.T) {
	ops := map[string]driver.Op{"a": driver.OpNone, "d": driver.OpNone}
	flow, sd := recordFixture(t, ops)
	rd := &errReader{scriptedDriver: sd}
	backend := promotedWithInventory(t, invEntry("r1", "gone"))

	res, err := Apply(context.Background(), flow, NewMemoryStore(), rd,
		ExecutorOptions{Backend: backend, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Warnings() {
		if strings.Contains(w, "inventory prune (node r1): boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", res.Warnings())
	}
	if len(rd.applied) != 0 {
		t.Errorf("applied = %v, want none", rd.applied)
	}
}

// kindRemovalFlow compiles a two-node flow where r1's desired set is
// parameterized: v1 gives it a ConfigMap, v2 drops the kind entirely
// (empty desired — the gap the label listing can never close).
func kindRemovalFlow(t *testing.T, r1Objs []driver.Object) *CompiledFlow {
	t.Helper()
	const src = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: rec}
spec:
  nodes:
    - id: r1
      type: res1
      config: {manifests: "kind: ConfigMap\nmetadata:\n  name: a\n"}
    - id: r2
      type: res2
      config: {manifests: "kind: ConfigMap\nmetadata:\n  name: d\n"}
`
	res1 := &resNodeType{name: "res1", objs: r1Objs}
	res2 := &resNodeType{name: "res2", objs: []driver.Object{cm("d")}}
	reg := NewRegistry()
	for _, nt := range []NodeType{res1, res2} {
		if err := reg.Register(nt); err != nil {
			t.Fatal(err)
		}
	}
	flow, err := compileWith(t, src, reg)
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

// TestKindRemovalPrunesAcrossApplies is the end-to-end gap closure
// (decision 8): apply v1 records the ConfigMap under r1, the flow then
// drops the kind so no node desires any ConfigMap of that node's, and
// the next apply's inventory prune — not the label listing, which sees
// no kinds to list — deletes the orphan and records an inventory
// without it. Double ObjectReader + in-memory backend; tests/ stays
// fake-driver-only.
func TestKindRemovalPrunesAcrossApplies(t *testing.T) {
	ctx := context.Background()
	lives := []driver.LiveObject{invLive("a"), invLive("d")}
	backend := &memBackend{}

	// v1: r1 desires the ConfigMap.
	v1 := kindRemovalFlow(t, []driver.Object{cm("a")})
	rd1 := newReaderDriver(map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate}, lives...)
	if _, err := Apply(ctx, v1, NewMemoryStore(), rd1,
		ExecutorOptions{Backend: backend, Prune: true}); err != nil {
		t.Fatalf("v1 apply: %v", err)
	}
	first, ok, err := backend.Get(ctx, 1)
	if err != nil || !ok || len(first.Inventory) != 2 {
		t.Fatalf("v1 record inventory = %+v (%v)", first.Inventory, err)
	}

	// v2: the kind is gone from r1's desired — only r2 still manages
	// its object.
	v2 := kindRemovalFlow(t, nil)
	rd2 := newReaderDriver(map[string]driver.Op{"d": driver.OpNone}, lives...)
	res, err := Apply(ctx, v2, NewMemoryStore(), rd2,
		ExecutorOptions{Backend: backend, Prune: true})
	if err != nil {
		t.Fatalf("v2 apply: %v", err)
	}
	for _, w := range res.Warnings() {
		t.Errorf("unexpected warning: %s", w)
	}
	// The orphan reaches the driver as a delete — scripted label
	// planning sees no desired objects, so only inventory prune can
	// produce it.
	if len(rd2.applied) != 1 {
		t.Fatalf("apply batches = %v", rd2.applied)
	}
	batch := rd2.applied[0]
	if len(batch) != 1 || batch[0].Op != driver.OpDelete || batch[0].Object.Name != "a" {
		t.Fatalf("batch = %+v, want the ConfigMap orphan", batch)
	}
	// The new record no longer claims it.
	second, ok, err := backend.Get(ctx, 2)
	if err != nil || !ok || second.Status != release.StatusSucceeded {
		t.Fatalf("v2 record = %+v (%v)", second, err)
	}
	if len(second.Inventory) != 1 || second.Inventory[0].Name != "d" ||
		second.Inventory[0].Node != "r2" {
		t.Errorf("v2 inventory = %+v, want only d@r2", second.Inventory)
	}
}
