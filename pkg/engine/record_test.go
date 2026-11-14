package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/release"
)

// recordFlow has two resource nodes, each carrying a manifests stream
// in its resolved config — enough to pin "one record per flow-apply",
// inventory union, and manifest capture.
const recordFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: rec}
spec:
  nodes:
    - id: r1
      type: res1
      config: {manifests: "kind: ConfigMap\nmetadata:\n  name: a\n"}
    - id: r2
      type: res2
      config: {manifests: "kind: Service\nmetadata:\n  name: d\n"}
`

// recordFixture compiles recordFlow with one desired object per
// resource node (a under r1, d under r2).
func recordFixture(t *testing.T, ops map[string]driver.Op) (*CompiledFlow, *scriptedDriver) {
	t.Helper()
	res1 := &resNodeType{name: "res1", objs: []driver.Object{waitObj("a")}}
	res2 := &resNodeType{name: "res2", objs: []driver.Object{waitObj("d")}}
	reg := NewRegistry()
	for _, nt := range []NodeType{res1, res2} {
		if err := reg.Register(nt); err != nil {
			t.Fatal(err)
		}
	}
	flow, err := compileWith(t, recordFlow, reg)
	if err != nil {
		t.Fatal(err)
	}
	return flow, &scriptedDriver{ops: ops}
}

// memBackend is an in-memory release.Backend: exact call counts,
// injectable failures, no IO. It implements the whole interface so it
// can stand in wherever the engine (or later stages) need one.
type memBackend struct {
	mu           sync.Mutex
	startCalls   int
	promoteCalls int
	started      []release.Record
	promoted     []release.Record
	nextSeq      int
	// takeover records the last Start's takeover flag.
	takeover   bool
	startErr   error
	promoteErr error
}

func (m *memBackend) Start(ctx context.Context, rec release.Record, takeover bool) (release.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startCalls++
	m.takeover = takeover
	if m.startErr != nil {
		return rec, m.startErr
	}
	m.nextSeq++
	rec.APIVersion = release.APIVersion
	rec.Sequence = m.nextSeq
	rec.Status = release.StatusPending
	rec.StartedAt = time.Now()
	m.started = append(m.started, rec)
	return rec, nil
}

func (m *memBackend) Promote(ctx context.Context, rec release.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.promoteCalls++
	if m.promoteErr != nil {
		return m.promoteErr
	}
	if rec.FinishedAt.IsZero() {
		rec.FinishedAt = time.Now()
	}
	m.promoted = append(m.promoted, rec)
	return nil
}

func (m *memBackend) Latest(ctx context.Context) (release.Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.promoted) == 0 {
		return release.Record{}, false, nil
	}
	return m.promoted[len(m.promoted)-1], true, nil
}

func (m *memBackend) List(ctx context.Context) ([]release.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]release.Record, len(m.promoted))
	copy(out, m.promoted)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (m *memBackend) Get(ctx context.Context, seq int) (release.Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.promoted {
		if r.Sequence == seq {
			return r, true, nil
		}
	}
	for _, r := range m.started {
		if r.Sequence == seq {
			return r, true, nil
		}
	}
	return release.Record{}, false, nil
}

// counts returns (startCalls, promoteCalls, started, promoted).
func (m *memBackend) counts() (int, int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCalls, m.promoteCalls, len(m.started), len(m.promoted)
}

// readerDriver adds driver.ObjectReader to scriptedDriver: canned
// live objects per test, NotFound omission included (absent name =
// not returned).
type readerDriver struct {
	*scriptedDriver
	mu    sync.Mutex
	lives map[string]driver.LiveObject
	reads int
}

func newReaderDriver(ops map[string]driver.Op, lives ...driver.LiveObject) *readerDriver {
	m := make(map[string]driver.LiveObject, len(lives))
	for _, l := range lives {
		m[l.Object.Name] = l
	}
	return &readerDriver{scriptedDriver: &scriptedDriver{ops: ops}, lives: m}
}

func (d *readerDriver) Read(ctx context.Context, owner driver.Owner, objs []driver.Object) ([]driver.LiveObject, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	out := make([]driver.LiveObject, 0, len(objs))
	for _, o := range objs {
		if l, ok := d.lives[o.Name]; ok {
			out = append(out, l)
		}
	}
	return out, nil
}

// TestApplyWritesOneRecordPerFlow: two resource nodes, one record —
// Start and Promote each fire exactly once per apply, the record is
// succeeded with node-attributed inventory (union of both nodes'
// desired objects) and both manifest streams in node order, and a
// second apply gets the next sequence.
func TestApplyWritesOneRecordPerFlow(t *testing.T) {
	flow, _ := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate})
	live := func(name, uid string) driver.LiveObject {
		return driver.LiveObject{
			Object: driver.Object{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: name},
			UID:    uid, Hash: "hash-" + name,
		}
	}
	rd := newReaderDriver(map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate},
		live("a", "uid-a"), live("d", "uid-d"))
	backend := &memBackend{}

	res, err := Apply(context.Background(), flow, NewMemoryStore(), rd, ExecutorOptions{Backend: backend})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if w := res.Warnings(); len(w) != 0 {
		t.Fatalf("warnings = %v, want none", w)
	}
	starts, promotes, started, promoted := backend.counts()
	if starts != 1 || promotes != 1 || started != 1 || promoted != 1 {
		t.Fatalf("startCalls/promoteCalls/started/promoted = %d/%d/%d/%d, want 1/1/1/1",
			starts, promotes, started, promoted)
	}
	rec := backend.promoted[0]
	if rec.Status != release.StatusSucceeded || rec.Trigger != release.TriggerManual {
		t.Fatalf("status/trigger = %q/%q", rec.Status, rec.Trigger)
	}
	if rec.Flow != "rec" || rec.Sequence != 1 {
		t.Fatalf("flow/seq = %q/%d, want rec/1", rec.Flow, rec.Sequence)
	}
	if rec.FinishedAt.IsZero() {
		t.Fatal("Promote did not set FinishedAt")
	}
	if rec.Error != "" {
		t.Errorf("succeeded record carries error %q", rec.Error)
	}

	// Inventory: union of both nodes' objects, each attributed to the
	// node that applied it, sorted node-then-name.
	if len(rec.Inventory) != 2 {
		t.Fatalf("inventory has %d entries, want 2: %+v", len(rec.Inventory), rec.Inventory)
	}
	byName := map[string]release.InventoryObject{}
	for _, inv := range rec.Inventory {
		byName[inv.Name] = inv
	}
	if inv := byName["a"]; inv.Node != "r1" || inv.UID != "uid-a" || inv.Hash != "hash-a" {
		t.Errorf("inventory[a] = %+v, want node r1 / uid-a / hash-a", inv)
	}
	if inv := byName["d"]; inv.Node != "r2" || inv.UID != "uid-d" {
		t.Errorf("inventory[d] = %+v, want node r2 / uid-d", inv)
	}

	// Manifests: both streams, node order (r1 before r2).
	if len(rec.Manifests) != 2 {
		t.Fatalf("manifests has %d streams, want 2: %+v", len(rec.Manifests), rec.Manifests)
	}
	if rec.Manifests[0].Node != "r1" || rec.Manifests[1].Node != "r2" {
		t.Errorf("manifest order = %s,%s, want r1,r2", rec.Manifests[0].Node, rec.Manifests[1].Node)
	}
	if !strings.Contains(rec.Manifests[0].YAML, "name: a") || !strings.Contains(rec.Manifests[1].YAML, "name: d") {
		t.Errorf("manifest payloads wrong: %+v", rec.Manifests)
	}

	// A second apply records the next sequence — one record per apply.
	if _, err := Apply(context.Background(), flow, NewMemoryStore(), rd, ExecutorOptions{Backend: backend}); err != nil {
		t.Fatalf("Apply 2: %v", err)
	}
	starts, promotes, _, promoted = backend.counts()
	if starts != 2 || promotes != 2 || promoted != 2 {
		t.Fatalf("second apply: startCalls/promoteCalls/promoted = %d/%d/%d, want 2/2/2",
			starts, promotes, promoted)
	}
	if seq := backend.promoted[1].Sequence; seq != 2 {
		t.Errorf("second record sequence = %d, want 2", seq)
	}
}

// readWaitDriver pairs readerDriver's live reads with a failing
// Waiter — a wait failure after a successful apply, the shape of a
// broken Deployment rollout.
type readWaitDriver struct {
	*readerDriver
	waitErr error
}

func (d *readWaitDriver) Wait(ctx context.Context, owner driver.Owner, objs []driver.Object, timeout time.Duration) error {
	return d.waitErr
}

// TestApplyFailedRecord: a failed run promotes a failed record that
// carries the run's error and the live inventory of what its apply
// put into the cluster (a later rollback must be able to prune those
// objects) — but no manifest streams: a failed release is never a
// rollback target.
func TestApplyFailedRecord(t *testing.T) {
	flow, _ := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpNone})
	live := driver.LiveObject{
		Object: driver.Object{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "a"},
		UID:    "uid-a", Hash: "hash-a",
	}
	rd := newReaderDriver(map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpNone}, live)
	dw := &readWaitDriver{readerDriver: rd, waitErr: errors.New("Deployment/web never became available")}
	backend := &memBackend{}

	res, err := Apply(context.Background(), flow, NewMemoryStore(), dw,
		ExecutorOptions{Wait: true, WaitTimeout: time.Minute, Backend: backend})
	if err == nil || !strings.Contains(err.Error(), "wait:") {
		t.Fatalf("err = %v, want wait: wrap", err)
	}
	starts, promotes, _, promoted := backend.counts()
	if starts != 1 || promotes != 1 || promoted != 1 {
		t.Fatalf("failed run must still record: startCalls/promoteCalls/promoted = %d/%d/%d",
			starts, promotes, promoted)
	}
	rec := backend.promoted[0]
	if rec.Status != release.StatusFailed {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if !strings.Contains(rec.Error, "wait: Deployment/web never became available") {
		t.Errorf("error = %q, want the wait failure", rec.Error)
	}
	if rec.Manifests != nil {
		t.Errorf("failed record must carry no manifests: %+v", rec.Manifests)
	}
	if len(rec.Inventory) != 1 || rec.Inventory[0].Name != "a" || rec.Inventory[0].Node != "r1" {
		t.Errorf("inventory = %+v, want the applied object a@r1", rec.Inventory)
	}
	if rec.FinishedAt.IsZero() {
		t.Fatal("FinishedAt not set on failure")
	}
	if res.Failed != true {
		t.Error("RunResult.Failed = false after a failed run")
	}
}

// TestApplyNilBackend: without a backend nothing is recorded and no
// warnings appear — the fake-driver CLI path stays silent.
func TestApplyNilBackend(t *testing.T) {
	flow, _ := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate})
	res, err := Apply(context.Background(), flow, NewMemoryStore(),
		&scriptedDriver{ops: map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate}},
		ExecutorOptions{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if w := res.Warnings(); len(w) != 0 {
		t.Fatalf("warnings = %v, want none", w)
	}
}

// TestApplyStartConcurrentRefusesBeforeRunning: a pending-record
// refusal fails the apply before any node runs and never promotes.
func TestApplyStartConcurrentRefusesBeforeRunning(t *testing.T) {
	flow, d := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate})
	backend := &memBackend{startErr: &release.ErrConcurrentApply{
		Flow: "rec", Sequence: 3, StartedAt: time.Now(),
	}}

	res, err := Apply(context.Background(), flow, NewMemoryStore(), d, ExecutorOptions{Backend: backend})
	var conc *release.ErrConcurrentApply
	if !errors.As(err, &conc) {
		t.Fatalf("err = %v, want *ErrConcurrentApply", err)
	}
	if conc.Sequence != 3 {
		t.Errorf("err sequence = %d, want 3", conc.Sequence)
	}
	startCalls, promoteCalls, started, _ := backend.counts()
	if startCalls != 1 || promoteCalls != 0 || started != 0 {
		t.Fatalf("startCalls/promoteCalls/started = %d/%d/%d, want 1/0/0",
			startCalls, promoteCalls, started)
	}
	if applied := d.applied; len(applied) != 0 {
		t.Fatalf("driver applied %d batches — nothing may run after a refusal", len(applied))
	}
	if nodes := res.Snapshot(); len(nodes) != 0 {
		t.Fatalf("refused apply recorded %d nodes, want 0", len(nodes))
	}
}

// TestApplyStartErrorWarnsAndProceeds: a non-concurrency backend
// failure degrades to a warning — the apply runs and succeeds without
// a record (Start failed, so there is nothing to promote).
func TestApplyStartErrorWarnsAndProceeds(t *testing.T) {
	flow, d := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate})
	backend := &memBackend{startErr: errors.New("secrets is forbidden")}

	res, err := Apply(context.Background(), flow, NewMemoryStore(), d, ExecutorOptions{Backend: backend})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	warns := res.Warnings()
	if len(warns) != 1 || !strings.Contains(warns[0], "release record not started") ||
		!strings.Contains(warns[0], "secrets is forbidden") {
		t.Fatalf("warnings = %v, want the start failure", warns)
	}
	startCalls, promoteCalls, started, promoted := backend.counts()
	if startCalls != 1 || promoteCalls != 0 || started != 0 || promoted != 0 {
		t.Fatalf("startCalls/promoteCalls/started/promoted = %d/%d/%d/%d, want 1/0/0/0",
			startCalls, promoteCalls, started, promoted)
	}
	if r, _ := res.Get("r1"); r.Status != StatusSucceeded {
		t.Errorf("r1 = %s, want succeeded", r.Status)
	}
}

// TestApplyPromoteErrorWarns: a failed promotion warns but never
// fails an apply that already finished.
func TestApplyPromoteErrorWarns(t *testing.T) {
	flow, d := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate})
	backend := &memBackend{promoteErr: errors.New("update conflict")}

	res, err := Apply(context.Background(), flow, NewMemoryStore(), d, ExecutorOptions{Backend: backend})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Two warnings: the scripted driver has no ObjectReader (no-reader
	// warning), and Promote then failed. Both are legitimate; the
	// second must never fail the apply.
	warns := res.Warnings()
	if len(warns) != 2 || !strings.Contains(warns[0], "does not report live state") ||
		!strings.Contains(warns[1], "release record: update conflict") {
		t.Fatalf("warnings = %v, want no-reader then promote failure", warns)
	}
	_, promoteCalls, _, promoted := backend.counts()
	if promoteCalls != 1 || promoted != 0 {
		t.Fatalf("promoteCalls/promoted = %d/%d, want 1/0 (attempted, not stored)", promoteCalls, promoted)
	}
	if r, _ := res.Get("r1"); r.Status != StatusSucceeded {
		t.Errorf("r1 = %s, want succeeded", r.Status)
	}
}

// TestApplyNoReaderEmptyInventoryWithWarning: a driver without
// ObjectReader still produces a succeeded record — manifests are
// captured from configs, inventory degrades to empty with a warning.
func TestApplyNoReaderEmptyInventoryWithWarning(t *testing.T) {
	flow, _ := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate})
	backend := &memBackend{}

	res, err := Apply(context.Background(), flow, NewMemoryStore(),
		&scriptedDriver{ops: map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate}},
		ExecutorOptions{Backend: backend})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	_, _, _, promoted := backend.counts()
	if promoted != 1 {
		t.Fatalf("promoted = %d, want 1", promoted)
	}
	rec := backend.promoted[0]
	if rec.Status != release.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", rec.Status)
	}
	if rec.Inventory != nil {
		t.Errorf("inventory = %+v, want empty", rec.Inventory)
	}
	if len(rec.Manifests) != 2 {
		t.Errorf("manifests = %+v, want both streams regardless of reader", rec.Manifests)
	}
	warns := res.Warnings()
	if len(warns) != 1 || !strings.Contains(warns[0], "does not report live state") {
		t.Fatalf("warnings = %v, want the no-reader warning", warns)
	}
}

// TestApplyReadFailureKeepsPartialInventory: one node's read failing
// warns and keeps the other node's inventory — partial beats none.
func TestApplyReadFailureKeepsPartialInventory(t *testing.T) {
	flow, _ := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate})
	rd := newReaderDriver(map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate},
		driver.LiveObject{
			Object: driver.Object{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "a"},
			UID:    "uid-a", Hash: "hash-a",
		})
	// Fail Read for the r2 owner only: r1's inventory survives.
	rd2 := &failingSecondReader{readerDriver: rd}

	backend := &memBackend{}
	res, err := Apply(context.Background(), flow, NewMemoryStore(), rd2, ExecutorOptions{Backend: backend})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec := backend.promoted[0]
	if len(rec.Inventory) != 1 || rec.Inventory[0].Name != "a" || rec.Inventory[0].Node != "r1" {
		t.Fatalf("inventory = %+v, want only r1's object", rec.Inventory)
	}
	warns := res.Warnings()
	if len(warns) != 1 || !strings.Contains(warns[0], `reading live state for node "r2"`) {
		t.Fatalf("warnings = %v, want the r2 read failure", warns)
	}
}

// failingSecondReader fails Read for the r2 owner only.
type failingSecondReader struct {
	*readerDriver
}

func (d *failingSecondReader) Read(ctx context.Context, owner driver.Owner, objs []driver.Object) ([]driver.LiveObject, error) {
	if owner.Node == "r2" {
		return nil, errors.New("forbidden")
	}
	return d.readerDriver.Read(ctx, owner, objs)
}

// TestApplyRecordOptionsPassThrough: Trigger and Takeover reach Start
// unchanged (the rollback path and --takeover depend on it).
func TestApplyRecordOptionsPassThrough(t *testing.T) {
	flow, d := recordFixture(t, map[string]driver.Op{"a": driver.OpCreate, "d": driver.OpCreate})
	backend := &memBackend{}

	if _, err := Apply(context.Background(), flow, NewMemoryStore(), d,
		ExecutorOptions{Backend: backend, Trigger: release.TriggerRollback, Takeover: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	backend.mu.Lock()
	takeover := backend.takeover
	backend.mu.Unlock()
	if !takeover {
		t.Error("Takeover did not reach Start")
	}
	if rec := backend.promoted[0]; rec.Trigger != release.TriggerRollback {
		t.Errorf("trigger = %q, want rollback", rec.Trigger)
	}
}
