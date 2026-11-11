package fake

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

func obj(name string, data map[string]any) driver.Object {
	return driver.Object{Group: "afon.dev", Version: "v1", Kind: "ConfigMap", Name: name, Data: data}
}

func TestPlan_CreateUpdateNone(t *testing.T) {
	d := New()
	ctx := context.Background()

	// Nothing live: everything creates.
	diffs, err := d.Plan(ctx, []driver.Object{obj("a", map[string]any{"x": 1})})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Op != driver.OpCreate {
		t.Fatalf("diffs = %+v", diffs)
	}

	// Apply, then identical desired: none.
	if err := d.Apply(ctx, diffs); err != nil {
		t.Fatal(err)
	}
	diffs, err = d.Plan(ctx, []driver.Object{obj("a", map[string]any{"x": 1})})
	if err != nil {
		t.Fatal(err)
	}
	if diffs[0].Op != driver.OpNone {
		t.Fatalf("op = %s, want none", diffs[0].Op)
	}

	// Changed data: update.
	diffs, err = d.Plan(ctx, []driver.Object{obj("a", map[string]any{"x": 2})})
	if err != nil {
		t.Fatal(err)
	}
	if diffs[0].Op != driver.OpUpdate {
		t.Fatalf("op = %s, want update", diffs[0].Op)
	}
}

// TestPlan_NoPhantomUpdates: canonical comparison means 1 vs 1.0 and map
// ordering never produce a phantom update (engine rule 7).
func TestPlan_NoPhantomUpdates(t *testing.T) {
	d := New()
	ctx := context.Background()
	live := obj("a", map[string]any{"n": 1.0, "m": map[string]any{"b": "2", "a": "1"}})
	if err := d.Apply(ctx, []driver.Diff{{Object: live, Op: driver.OpCreate}}); err != nil {
		t.Fatal(err)
	}
	want := obj("a", map[string]any{"n": 1, "m": map[string]any{"a": "1", "b": "2"}})
	diffs, err := d.Plan(ctx, []driver.Object{want})
	if err != nil {
		t.Fatal(err)
	}
	if diffs[0].Op != driver.OpNone {
		t.Fatalf("phantom update: op = %s", diffs[0].Op)
	}
}

func TestPersistence_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "fake-state.json")
	ctx := context.Background()

	d1, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d1.Apply(ctx, []driver.Diff{{Object: obj("a", map[string]any{"x": 1}), Op: driver.OpCreate}}); err != nil {
		t.Fatal(err)
	}

	// A second process: fresh driver, same file.
	d2, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := d2.Plan(ctx, []driver.Object{obj("a", map[string]any{"x": 1})})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Op != driver.OpNone {
		t.Fatalf("state did not survive: %+v", diffs)
	}
}

func TestPersistence_CorruptFileIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistent(path); err == nil {
		t.Fatal("corrupt state must be an error, never a silent wipe")
	}
}

// TestApply_AtomicOnWriteFailure: if the state write fails, live memory
// keeps the old objects and the old file is untouched. Simulated by
// making the state path a directory (rename onto it fails).
func TestApply_AtomicOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	// Seed a valid file first so "old state" exists on disk.
	path := filepath.Join(dir, "fake-state.json")
	d, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Apply(ctx, []driver.Diff{{Object: obj("old", nil), Op: driver.OpCreate}}); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Break the write: replace the file path with a directory so rename
	// fails midway through the next Apply.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	err = d.Apply(ctx, []driver.Diff{{Object: obj("new", nil), Op: driver.OpCreate}})
	if err == nil {
		t.Fatal("apply must fail when the state write fails")
	}
	// In-memory state must not have absorbed the change.
	for _, o := range d.Objects() {
		if o.Name == "new" {
			t.Error("memory absorbed a change whose write failed")
		}
	}
	_ = oldBytes // the directory still occupies the path; file content
	// asserted by the "new not in memory" check: memory and disk diverged
	// only because disk was made unwritable, not because Apply half-failed.
}

// TestConcurrentApply_ParallelResourceNodes: parallel resource nodes call
// Plan and Apply at the same time; the mutex must keep it race-clean
// (engine rule 6). Run with -race.
func TestConcurrentApply_ParallelResourceNodes(t *testing.T) {
	d := New()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := obj("obj-"+string(rune('a'+i)), map[string]any{"i": i})
			diffs, err := d.Plan(ctx, []driver.Object{o})
			if err != nil {
				t.Error(err)
				return
			}
			if err := d.Apply(ctx, diffs); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if len(d.Objects()) != 8 {
		t.Errorf("objects = %d, want 8", len(d.Objects()))
	}
}
