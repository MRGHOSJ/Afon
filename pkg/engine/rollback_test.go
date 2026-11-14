package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/release"
)

// rollbackManifest is a real YAML stream for K8sApply to parse — the
// shape a k8s-apply node records.
const rollbackManifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: demo
data:
  k: v1
`

// seedRec builds a restorable record for flow name "rec" with one r1
// manifest stream.
func seedRec(seq int, mutate func(*release.Record)) release.Record {
	rec := release.Record{
		APIVersion: release.APIVersion,
		Flow:       "rec",
		Sequence:   seq,
		Status:     release.StatusSucceeded,
		Trigger:    release.TriggerManual,
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Manifests:  []release.ManifestStream{{Node: "r1", YAML: rollbackManifest}},
	}
	if mutate != nil {
		mutate(&rec)
	}
	return rec
}

// TestRollbackPlanPlaysRecordedManifests: the plan replays release 1's
// manifest as node r1 — create/update for drifted live state, plus the
// inventory prune that deletes what the newer release (2) owns and
// release 1 never had.
func TestRollbackPlanPlaysRecordedManifests(t *testing.T) {
	flow, _ := recordFixture(t, nil)
	m := &memBackend{}
	if err := m.Promote(context.Background(), seedRec(1, func(r *release.Record) {
		r.Inventory = []release.InventoryObject{invEntry("r1", "a")}
	})); err != nil {
		t.Fatal(err)
	}
	if err := m.Promote(context.Background(), seedRec(2, func(r *release.Record) {
		r.Inventory = []release.InventoryObject{invEntry("r1", "a"), invEntry("r1", "extra")}
	})); err != nil {
		t.Fatal(err)
	}
	rd := newReaderDriver(map[string]driver.Op{"a": driver.OpUpdate},
		invLive("a"), invLive("extra"))

	p, err := RollbackPlan(context.Background(), flow, m, rd, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Flow != flow.Name {
		t.Errorf("plan flow = %q", p.Flow)
	}
	if len(p.Entries) != 1 {
		t.Fatalf("entries = %d (%+v)", len(p.Entries), p.Entries)
	}
	e := p.Entries[0]
	if e.NodeID != "r1" || e.Status != PlanDelete {
		t.Fatalf("entry = %+v", e)
	}
	if len(e.Diffs) != 2 {
		t.Fatalf("diffs = %+v", e.Diffs)
	}
	if e.Diffs[0].Op != driver.OpUpdate || e.Diffs[0].Object.Name != "a" {
		t.Errorf("diff[0] = %+v", e.Diffs[0])
	}
	if e.Diffs[1].Op != driver.OpDelete || e.Diffs[1].Object.Name != "extra" {
		t.Errorf("diff[1] = %+v", e.Diffs[1])
	}
}

// TestRollbackPlanRefuses pins the refusal paths: a backend is
// required, the sequence must exist and belong to this flow, and only
// restorable releases with manifests can be replayed.
func TestRollbackPlanRefuses(t *testing.T) {
	flow, _ := recordFixture(t, nil)
	seed := func(mutate func(*release.Record)) *memBackend {
		m := &memBackend{}
		if err := m.Promote(context.Background(), seedRec(1, mutate)); err != nil {
			t.Fatal(err)
		}
		return m
	}
	cases := []struct {
		name    string
		backend release.Backend
		seq     int
		wantSub string
	}{
		{"nil backend", nil, 1, "rollback needs a release backend"},
		{"zero sequence", seed(nil), 0, "release sequence must be >= 1"},
		{"missing sequence", seed(nil), 5, "release v5 does not exist"},
		{"wrong flow", seed(func(r *release.Record) { r.Flow = "other" }), 1,
			`belongs to flow "other"`},
		{"failed status", seed(func(r *release.Record) {
			r.Status = release.StatusFailed
			r.Error = "boom"
		}), 1, `status "failed" has nothing to restore`},
		{"pending status", seed(func(r *release.Record) {
			r.Status = release.StatusPending
		}), 1, `status "pending" has nothing to restore`},
		{"no manifests", seed(func(r *release.Record) { r.Manifests = nil }), 1,
			"no manifests recorded"},
		{"only empty streams", seed(func(r *release.Record) {
			r.Manifests = []release.ManifestStream{{Node: "r1", YAML: " "}}
		}), 1, "no manifests recorded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RollbackPlan(context.Background(), flow, tc.backend,
				newReaderDriver(nil), tc.seq)
			if err == nil {
				t.Fatal("RollbackPlan: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestRollbackExecutesAndRecords: rollback runs the normal apply path
// against the replay flow — the drift update and the inventory prune
// delete both reach the driver — and writes one new append-only record:
// trigger rollback, rolledBackTo the target, sequence after the
// source, K's manifests and the fresh inventory captured.
func TestRollbackExecutesAndRecords(t *testing.T) {
	flow, _ := recordFixture(t, nil)
	m := &memBackend{}
	if err := m.Promote(context.Background(), seedRec(1, func(r *release.Record) {
		r.Inventory = []release.InventoryObject{invEntry("r1", "a")}
	})); err != nil {
		t.Fatal(err)
	}
	if err := m.Promote(context.Background(), seedRec(2, func(r *release.Record) {
		r.Inventory = []release.InventoryObject{invEntry("r1", "a"), invEntry("r1", "extra")}
	})); err != nil {
		t.Fatal(err)
	}
	m.nextSeq = 2 // seeded sequences 1 and 2: the rollback becomes v3
	rd := newReaderDriver(map[string]driver.Op{"a": driver.OpUpdate},
		invLive("a"), invLive("extra"))

	res, err := Rollback(context.Background(), flow, NewMemoryStore(), rd, 1,
		ExecutorOptions{Backend: m, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rd.applied) != 1 {
		t.Fatalf("apply batches = %v", rd.applied)
	}
	batch := rd.applied[0]
	if len(batch) != 2 || batch[0].Op != driver.OpUpdate || batch[1].Op != driver.OpDelete {
		t.Errorf("batch = %+v", batch)
	}
	if batch[1].Object.Name != "extra" {
		t.Errorf("pruned = %v, want extra", batch[1].Object)
	}
	for _, w := range res.Warnings() {
		t.Errorf("unexpected warning: %s", w)
	}
	if len(m.promoted) != 3 {
		t.Fatalf("promoted = %d, want 3", len(m.promoted))
	}
	rec := m.promoted[2]
	if rec.Sequence != 3 {
		t.Errorf("sequence = %d", rec.Sequence)
	}
	if rec.Trigger != release.TriggerRollback {
		t.Errorf("trigger = %q", rec.Trigger)
	}
	if rec.RolledBackTo != 1 {
		t.Errorf("rolledBackTo = %d", rec.RolledBackTo)
	}
	if rec.Status != release.StatusSucceeded {
		t.Errorf("status = %q", rec.Status)
	}
	if len(rec.Manifests) != 1 || rec.Manifests[0].Node != "r1" ||
		rec.Manifests[0].YAML != rollbackManifest {
		t.Errorf("manifests = %+v", rec.Manifests)
	}
	if len(rec.Inventory) != 1 || rec.Inventory[0].Name != "a" {
		t.Errorf("inventory = %+v", rec.Inventory)
	}
}

// TestDefaultRollbackTarget: no records is an error, a lone record has
// nothing earlier to target, and the default skips the latest record
// for the newest restorable one below it.
func TestDefaultRollbackTarget(t *testing.T) {
	ctx := context.Background()

	if _, err := DefaultRollbackTarget(ctx, &memBackend{}, "f"); err == nil ||
		!strings.Contains(err.Error(), `no releases recorded for flow "f"`) {
		t.Errorf("empty backend err = %v", err)
	}

	solo := &memBackend{}
	if err := solo.Promote(ctx, seedRec(1, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultRollbackTarget(ctx, solo, "f"); err == nil ||
		!strings.Contains(err.Error(), "no earlier release") {
		t.Errorf("solo err = %v", err)
	}

	m := &memBackend{}
	// Promote oldest first: v1 succeeded, v2 succeeded, v3 failed.
	if err := m.Promote(ctx, seedRec(1, func(r *release.Record) {
		r.Status = release.StatusSucceeded
	})); err != nil {
		t.Fatal(err)
	}
	if err := m.Promote(ctx, seedRec(2, nil)); err != nil {
		t.Fatal(err)
	}
	if err := m.Promote(ctx, seedRec(3, func(r *release.Record) {
		r.Status = release.StatusFailed
	})); err != nil {
		t.Fatal(err)
	}
	// Latest (v3) is failed and skipped; v2 is the newest restorable
	// below it.
	got, err := DefaultRollbackTarget(ctx, m, "f")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("target = %d, want 2", got)
	}
}

// autoRollbackFixture compiles a one-node flow whose node declares
// onFailure: rollback.
func autoRollbackFixture(t *testing.T) *CompiledFlow {
	t.Helper()
	res1 := &resNodeType{name: "res1", objs: []driver.Object{waitObj("a")}}
	reg := NewRegistry()
	if err := reg.Register(res1); err != nil {
		t.Fatal(err)
	}
	const src = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: rec}
spec:
  nodes:
    - id: r1
      type: res1
      onFailure: rollback
      config: {manifests: "kind: ConfigMap\nmetadata:\n  name: a\n"}
`
	flow, err := compileWith(t, src, reg)
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

// failedRun builds a run result with the given nodes failed.
func failedRun(nodes ...string) *RunResult {
	res := newRunResult()
	for _, id := range nodes {
		res.Record(NodeResult{NodeID: id, Status: StatusFailed, Err: errors.New("boom")})
	}
	return res
}

// TestAutoRollbackTarget: auto-rollback arms only when the run failed
// on a node declaring onFailure: rollback and a backend exists to
// replay from; the target is the newest restorable release below the
// latest record (the failed run's own record).
func TestAutoRollbackTarget(t *testing.T) {
	ctx := context.Background()
	armed := autoRollbackFixture(t)
	// recordFixture's nodes carry the halt default.
	haltFlow, _ := recordFixture(t, nil)

	// A failed run's records: v1 succeeded earlier, v2 is the failed
	// run's own record (latest, skipped by the default target).
	failedThenPrior := func(t *testing.T) *memBackend {
		t.Helper()
		m := &memBackend{}
		if err := m.Promote(ctx, seedRec(1, nil)); err != nil {
			t.Fatal(err)
		}
		if err := m.Promote(ctx, seedRec(2, func(r *release.Record) {
			r.Status = release.StatusFailed
		})); err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("no backend", func(t *testing.T) {
		got, err := AutoRollbackTarget(ctx, armed, nil, failedRun("r1"))
		if err != nil || got != 0 {
			t.Errorf("got = %d, %v; want 0, nil", got, err)
		}
	})
	t.Run("nil result", func(t *testing.T) {
		got, err := AutoRollbackTarget(ctx, armed, failedThenPrior(t), nil)
		if err != nil || got != 0 {
			t.Errorf("got = %d, %v; want 0, nil", got, err)
		}
	})
	t.Run("successful run", func(t *testing.T) {
		res := newRunResult()
		res.Record(NodeResult{NodeID: "r1", Status: StatusSucceeded})
		got, err := AutoRollbackTarget(ctx, armed, failedThenPrior(t), res)
		if err != nil || got != 0 {
			t.Errorf("got = %d, %v; want 0, nil", got, err)
		}
	})
	t.Run("failed node without onFailure", func(t *testing.T) {
		got, err := AutoRollbackTarget(ctx, haltFlow, failedThenPrior(t), failedRun("r1"))
		if err != nil || got != 0 {
			t.Errorf("got = %d, %v; want 0, nil", got, err)
		}
	})
	t.Run("armed", func(t *testing.T) {
		got, err := AutoRollbackTarget(ctx, armed, failedThenPrior(t), failedRun("r1"))
		if err != nil {
			t.Fatal(err)
		}
		if got != 1 {
			t.Errorf("target = %d, want 1", got)
		}
	})
	t.Run("armed but first release failed", func(t *testing.T) {
		m := &memBackend{}
		if err := m.Promote(ctx, seedRec(1, func(r *release.Record) {
			r.Status = release.StatusFailed
		})); err != nil {
			t.Fatal(err)
		}
		_, err := AutoRollbackTarget(ctx, armed, m, failedRun("r1"))
		if err == nil || !strings.Contains(err.Error(), "no earlier release") {
			t.Errorf("err = %v", err)
		}
	})
}
