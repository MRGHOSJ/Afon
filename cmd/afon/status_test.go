package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/release"
)

// stubStatusDriver is a driver.Driver + ObjectReader + ObjectLister
// for status tests: canned live objects and flow-owned objects, no
// cluster.
type stubStatusDriver struct {
	lives []driver.LiveObject
	owned []driver.Object
}

func (s *stubStatusDriver) Plan(ctx context.Context, owner driver.Owner, desired []driver.Object) ([]driver.Diff, error) {
	return nil, nil
}

func (s *stubStatusDriver) Apply(ctx context.Context, owner driver.Owner, diffs []driver.Diff) error {
	return nil
}

// Read returns the canned lives restricted to what was asked for —
// the real driver's NotFound-omission contract.
func (s *stubStatusDriver) Read(ctx context.Context, owner driver.Owner, objs []driver.Object) ([]driver.LiveObject, error) {
	want := map[string]bool{}
	for _, o := range objs {
		want[invKey(o.Group, o.Kind, o.Namespace, o.Name)] = true
	}
	var out []driver.LiveObject
	for _, l := range s.lives {
		if want[invKey(l.Object.Group, l.Object.Kind, l.Object.Namespace, l.Object.Name)] {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *stubStatusDriver) ListOwned(ctx context.Context, owner driver.Owner, kinds []driver.ObjectKind) ([]driver.Object, error) {
	return s.owned, nil
}

// bareDriver implements only driver.Driver — no reader, no lister.
type bareDriver struct{}

func (bareDriver) Plan(ctx context.Context, owner driver.Owner, desired []driver.Object) ([]driver.Diff, error) {
	return nil, nil
}
func (bareDriver) Apply(ctx context.Context, owner driver.Owner, diffs []driver.Diff) error {
	return nil
}

func invObj(node, name, uid, hash string) release.InventoryObject {
	return release.InventoryObject{
		Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: name,
		Node: node, UID: uid, Hash: hash,
	}
}

func liveObj(name, uid, hash string) driver.LiveObject {
	return driver.LiveObject{
		Object: driver.Object{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: name},
		UID:    uid, Hash: hash,
	}
}

func succeededRecord(seq int, inv ...release.InventoryObject) release.Record {
	return release.Record{
		APIVersion: release.APIVersion, Flow: "web", Sequence: seq,
		Status: release.StatusSucceeded, Trigger: release.TriggerManual,
		StartedAt: time.Now().Add(-2 * time.Minute),
		Inventory: inv,
	}
}

func TestStatusRequiresK8sDriver(t *testing.T) {
	old := flagDriver
	flagDriver = "fake"
	t.Cleanup(func() { flagDriver = old })

	err := runStatus(context.Background())
	if err == nil || err.Error() != errReleaseRecordsNeedK8s.Error() {
		t.Fatalf("err = %v, want the k8s-driver gate", err)
	}
}

func TestStatusOutputInfoLines(t *testing.T) {
	ctx := context.Background()

	t.Run("no records", func(t *testing.T) {
		out, err := statusOutput(ctx, bareDriver{}, nil, "web", "text")
		if err != nil {
			t.Fatalf("statusOutput: %v", err)
		}
		if out != "no releases recorded for flow \"web\"\n" {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("pending latest", func(t *testing.T) {
		pending := succeededRecord(7)
		pending.Status = release.StatusPending
		pending.Inventory = nil
		out, err := statusOutput(ctx, bareDriver{}, []release.Record{pending}, "web", "text")
		if err != nil {
			t.Fatalf("statusOutput: %v", err)
		}
		if !strings.Contains(out, "release v7 is pending") {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("failed latest", func(t *testing.T) {
		failed := succeededRecord(8)
		failed.Status = release.StatusFailed
		failed.Inventory = nil
		out, err := statusOutput(ctx, bareDriver{}, []release.Record{failed}, "web", "text")
		if err != nil {
			t.Fatalf("statusOutput: %v", err)
		}
		if !strings.Contains(out, "release v8: failed (not restorable") {
			t.Fatalf("out = %q", out)
		}
	})
}

func TestStatusOutputNeedsReaderAndLister(t *testing.T) {
	rec := succeededRecord(3, invObj("r1", "a", "uid-a", "h1"))
	_, err := statusOutput(context.Background(), bareDriver{}, []release.Record{rec}, "web", "text")
	if err == nil || !strings.Contains(err.Error(), "does not report live state") {
		t.Fatalf("err = %v, want the reader error", err)
	}
}

func TestStatusOutputComparisons(t *testing.T) {
	// Inventory: a ok, b changed, c recreated, d missing; live adds
	// flow-owned orphan (extra).
	inv := []release.InventoryObject{
		invObj("r1", "a", "uid-a", "h-a"),
		invObj("r1", "b", "uid-b", "h-b"),
		invObj("r2", "c", "uid-c", "h-c"),
		invObj("r2", "d", "uid-d", "h-d"),
	}
	stub := &stubStatusDriver{
		lives: []driver.LiveObject{
			liveObj("a", "uid-a", "h-a"),
			liveObj("b", "uid-b", "h-b2"),
			liveObj("c", "uid-c2", "h-c"),
		},
		owned: []driver.Object{
			{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "a"},
			{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "orphan"},
		},
	}
	rec := succeededRecord(3, inv...)

	out, err := statusOutput(context.Background(), stub, []release.Record{rec}, "web", "text")
	if err != nil {
		t.Fatalf("statusOutput: %v", err)
	}
	want := "" +
		"release v3: succeeded (manual, started 2m ago)\n" +
		"  ok 1\n" +
		"  missing 1:\n" +
		"    ConfigMap/demo/d\n" +
		"  changed 1:\n" +
		"    ConfigMap/demo/b\n" +
		"  recreated 1:\n" +
		"    ConfigMap/demo/c\n" +
		"  extra 1:\n" +
		"    ConfigMap/demo/orphan\n"
	if out != want {
		t.Fatalf("text:\n%s\nwant:\n%s", out, want)
	}

	// JSON carries the disagreements: hashes for changed, UIDs for
	// recreated.
	jsonOut, err := statusOutput(context.Background(), stub, []release.Record{rec}, "web", "json")
	if err != nil {
		t.Fatalf("statusOutput json: %v", err)
	}
	var rep statusReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, jsonOut)
	}
	if rep.Release.Sequence != 3 || rep.Release.Status != release.StatusSucceeded {
		t.Fatalf("release = %+v", rep.Release)
	}
	if len(rep.OK) != 1 || rep.OK[0].Name != "a" || rep.OK[0].Node != "r1" {
		t.Errorf("ok = %+v", rep.OK)
	}
	if len(rep.Changed) != 1 || rep.Changed[0].RecordedHash != "h-b" || rep.Changed[0].LiveHash != "h-b2" {
		t.Errorf("changed = %+v", rep.Changed)
	}
	if len(rep.Recreated) != 1 || rep.Recreated[0].UID != "uid-c" || rep.Recreated[0].LiveUID != "uid-c2" {
		t.Errorf("recreated = %+v", rep.Recreated)
	}
	if len(rep.Missing) != 1 || rep.Missing[0].Name != "d" {
		t.Errorf("missing = %+v", rep.Missing)
	}
	if len(rep.Extra) != 1 || rep.Extra[0].Name != "orphan" {
		t.Errorf("extra = %+v", rep.Extra)
	}
}

func TestStatusOutputAllInSync(t *testing.T) {
	inv := []release.InventoryObject{
		invObj("r1", "a", "uid-a", "h-a"),
		invObj("r2", "b", "uid-b", "h-b"),
	}
	stub := &stubStatusDriver{
		lives: []driver.LiveObject{liveObj("a", "uid-a", "h-a"), liveObj("b", "uid-b", "h-b")},
		owned: []driver.Object{{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "a"}},
	}
	out, err := statusOutput(context.Background(), stub, []release.Record{succeededRecord(9, inv...)}, "web", "text")
	if err != nil {
		t.Fatalf("statusOutput: %v", err)
	}
	want := "release v9: succeeded (manual, started 2m ago)\n  ok 2\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestCompareInventoryIgnoresVersion(t *testing.T) {
	// The record stores the version the API served then; a preferred
	// version change must not fake a missing object.
	inv := []release.InventoryObject{{
		Group: "apps", Version: "v1beta1", Kind: "Deployment",
		Namespace: "demo", Name: "web", Node: "r1", UID: "uid", Hash: "h",
	}}
	lives := []driver.LiveObject{{
		Object: driver.Object{Group: "apps", Version: "v1", Kind: "Deployment",
			Namespace: "demo", Name: "web"},
		UID: "uid", Hash: "h",
	}}
	rep := compareInventory(inv, lives, nil)
	if len(rep.OK) != 1 {
		t.Fatalf("ok = %+v, want the object matched across versions", rep.OK)
	}
	if len(rep.Missing) != 0 {
		t.Fatalf("missing = %+v, want none", rep.Missing)
	}
}

func TestStatusUnknownOutput(t *testing.T) {
	if _, err := formatStatus(&statusReport{}, "yaml"); err == nil ||
		!strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("err = %v, want unknown output format", err)
	}
}

func TestRecordKindsDedupesAndSorts(t *testing.T) {
	rec1 := succeededRecord(1,
		invObj("r1", "a", "u", "h"),
		release.InventoryObject{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "d", Name: "w", Node: "r1", UID: "u", Hash: "h"},
	)
	rec2 := succeededRecord(2, invObj("r2", "b", "u", "h"))
	kinds := recordKinds([]release.Record{rec1, rec2})
	want := []driver.ObjectKind{{Kind: "ConfigMap"}, {Group: "apps", Kind: "Deployment"}}
	if len(kinds) != 2 || kinds[0] != want[0] || kinds[1] != want[1] {
		t.Fatalf("kinds = %+v, want %+v (deduped, sorted: ConfigMap before apps/Deployment)", kinds, want)
	}
}
