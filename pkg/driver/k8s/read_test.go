package k8s

import (
	"context"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readCM builds a ConfigMap as a previous afon apply would have left
// it: owner labels, a UID, and a resourceVersion the hash must ignore.
func readCM(name, uid string, owner driver.Owner, data map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       testNS,
			"uid":             uid,
			"resourceVersion": "99",
			"labels":          map[string]any{flowLabel: owner.Flow, nodeLabel: owner.Node},
		},
	}
	if data != nil {
		obj["data"] = data
	}
	return &unstructured.Unstructured{Object: obj}
}

// readSvc is readCM's Service sibling for ListOwned fixtures.
func readSvc(name, uid string, owner driver.Owner) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"name":      name,
			"namespace": testNS,
			"uid":       uid,
			"labels":    map[string]any{flowLabel: owner.Flow, nodeLabel: owner.Node},
		},
	}}
}

func TestReadReturnsUIDAndStableHash(t *testing.T) {
	d, _ := newTestDriver(t, false, nil, readCM("a", "uid-123", testOwner, map[string]any{"k": "v"}))
	ctx := context.Background()

	// Namespace defaults to the driver's when the object omits it.
	obj := driver.Object{Kind: "ConfigMap", Name: "a", Data: map[string]any{"k": "v"}}
	got, err := d.Read(ctx, testOwner, []driver.Object{obj})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d live objects, want 1", len(got))
	}
	live := got[0]
	if live.UID != "uid-123" {
		t.Errorf("UID = %q, want uid-123", live.UID)
	}
	if live.Object.Kind != "ConfigMap" || live.Object.Name != "a" || live.Object.Namespace != testNS {
		t.Errorf("identity = %+v", live.Object)
	}
	if len(live.Hash) != 64 {
		t.Errorf("hash = %q, want 64 hex chars", live.Hash)
	}
	again, err := d.Read(ctx, testOwner, []driver.Object{obj})
	if err != nil {
		t.Fatalf("Read again: %v", err)
	}
	if again[0].Hash != live.Hash {
		t.Errorf("hash not stable across reads: %q vs %q", live.Hash, again[0].Hash)
	}
}

func TestReadHashIgnoresVolatile(t *testing.T) {
	// The SAME object read before and after its volatile keys move
	// (uid, resourceVersion, status) must hash equal — volatile keys
	// never participate (design rule 7).
	d, dyn := newTestDriver(t, false, nil, readCM("x", "uid-a", testOwner, map[string]any{"k": "v"}))
	ctx := context.Background()
	obj := driver.Object{Kind: "ConfigMap", Name: "x"}

	before, err := d.Read(ctx, testOwner, []driver.Object{obj})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	live, err := dyn.Resource(cmGVR).Namespace(testNS).Get(ctx, "x", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	live.SetUID("uid-b")
	live.SetResourceVersion("42")
	live.Object["status"] = map[string]any{"phase": "Ready"}
	if _, err := dyn.Resource(cmGVR).Namespace(testNS).Update(ctx, live, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, err := d.Read(ctx, testOwner, []driver.Object{obj})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if after[0].UID != "uid-b" || after[0].ResourceVersion != "42" {
		t.Fatalf("volatile fields not reported: %+v", after[0])
	}
	if before[0].Hash != after[0].Hash {
		t.Errorf("hash moved with volatile keys: %q vs %q", before[0].Hash, after[0].Hash)
	}
}

func TestReadHashChangesWithContent(t *testing.T) {
	d, dyn := newTestDriver(t, false, nil, readCM("a", "uid-1", testOwner, map[string]any{"k": "v1"}))
	ctx := context.Background()
	obj := driver.Object{Kind: "ConfigMap", Name: "a"}

	before, err := d.Read(ctx, testOwner, []driver.Object{obj})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	live, err := dyn.Resource(cmGVR).Namespace(testNS).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := unstructured.SetNestedMap(live.Object, map[string]any{"k": "v2"}, "data"); err != nil {
		t.Fatalf("SetNestedMap: %v", err)
	}
	if _, err := dyn.Resource(cmGVR).Namespace(testNS).Update(ctx, live, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, err := d.Read(ctx, testOwner, []driver.Object{obj})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if before[0].Hash == after[0].Hash {
		t.Error("hash unchanged after content change")
	}
}

func TestReadOmitsNotFound(t *testing.T) {
	d, _ := newTestDriver(t, false, nil, readCM("a", "uid-1", testOwner, map[string]any{"k": "v"}))
	got, err := d.Read(context.Background(), testOwner, []driver.Object{
		{Kind: "ConfigMap", Name: "a"},
		{Kind: "ConfigMap", Name: "missing"},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].Object.Name != "a" {
		t.Fatalf("got %+v, want only a — NotFound must be omitted, not an error", got)
	}
}

func TestReadOmitsForeignOwner(t *testing.T) {
	foreign := driver.Owner{Flow: "other", Node: "apply"}
	d, _ := newTestDriver(t, false, nil, readCM("a", "uid-1", foreign, map[string]any{"k": "v"}))
	got, err := d.Read(context.Background(), testOwner, []driver.Object{
		{Kind: "ConfigMap", Name: "a"},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none — a foreign UID must never enter inventory", got)
	}
}

func TestReadEmptyAndClusterScoped(t *testing.T) {
	d, _ := newTestDriver(t, false, nil)
	ctx := context.Background()

	got, err := d.Read(ctx, testOwner, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Read(nil): got %d err %v, want 0/nil", len(got), err)
	}

	_, err = d.Read(ctx, testOwner, []driver.Object{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Name: "x"},
	})
	if err == nil {
		t.Fatal("Read of cluster-scoped kind: want ErrClusterScoped, got nil")
	}
}

func TestListOwned(t *testing.T) {
	// Two ConfigMaps for this flow on different nodes, one Service for
	// this flow, one ConfigMap owned by another flow, one unlabeled
	// ConfigMap. Only the three this flow owns may come back.
	other := driver.Owner{Flow: "other", Node: "apply"}
	d, _ := newTestDriver(t, false, nil,
		readCM("a", "uid-a", testOwner, nil),
		readCM("b", "uid-b", driver.Owner{Flow: testFlow, Node: "tail"}, nil),
		readCM("c", "uid-c", other, nil),
		readCM("unlabeled", "uid-u", driver.Owner{}, nil),
		readSvc("svc", "uid-s", testOwner),
	)
	ctx := context.Background()
	kinds := []driver.ObjectKind{{Kind: "ConfigMap"}, {Kind: "Service"}}

	t.Run("flow wide", func(t *testing.T) {
		got, err := d.ListOwned(ctx, driver.Owner{Flow: testFlow}, kinds)
		if err != nil {
			t.Fatalf("ListOwned: %v", err)
		}
		names := map[string]bool{}
		for _, o := range got {
			names[o.Name] = true
		}
		want := map[string]bool{"a": true, "b": true, "svc": true}
		if len(names) != len(want) {
			t.Fatalf("owned = %v, want %v (plus: other-flow and unlabeled excluded)", names, want)
		}
		for n := range want {
			if !names[n] {
				t.Errorf("missing owned %q", n)
			}
		}
	})

	t.Run("node scoped", func(t *testing.T) {
		// testOwner is node "apply": both a and svc carry it, b (node
		// tail) must not.
		got, err := d.ListOwned(ctx, testOwner, kinds)
		if err != nil {
			t.Fatalf("ListOwned: %v", err)
		}
		names := map[string]bool{}
		for _, o := range got {
			names[o.Name] = true
		}
		if len(got) != 2 || !names["a"] || !names["svc"] {
			t.Fatalf("node-apply owned = %+v, want a and svc", got)
		}
	})

	t.Run("unknown kind skipped", func(t *testing.T) {
		got, err := d.ListOwned(ctx, driver.Owner{Flow: testFlow},
			[]driver.ObjectKind{{Kind: "ConfigMap"}, {Group: "gone.example.com", Kind: "Widget"}})
		if err != nil {
			t.Fatalf("ListOwned with unservable kind: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d objects, want the 2 ConfigMaps", len(got))
		}
	})

	t.Run("empty kinds", func(t *testing.T) {
		got, err := d.ListOwned(ctx, driver.Owner{Flow: testFlow}, nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("got %d err %v, want 0/nil", len(got), err)
		}
	})

	t.Run("flow required", func(t *testing.T) {
		if _, err := d.ListOwned(ctx, driver.Owner{}, kinds); err == nil {
			t.Fatal("empty flow: want error, got nil")
		}
	})
}
