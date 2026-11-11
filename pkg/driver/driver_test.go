package driver

import (
	"testing"
)

func TestCanonical_MapOrder(t *testing.T) {
	a := map[string]any{"b": 1, "a": 2, "c": map[string]any{"z": true, "y": "s"}}
	b := map[string]any{"c": map[string]any{"y": "s", "z": true}, "a": 2, "b": 1}
	ca, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Errorf("map order changed canonical form:\n%s\n%s", ca, cb)
	}
}

func TestCanonical_OneVsOnePointZero(t *testing.T) {
	// yaml decodes 1 as int and 1.0 as float64; both must hash alike.
	a := map[string]any{"n": 1}
	b := map[string]any{"n": 1.0}
	ca, _ := Canonical(a)
	cb, _ := Canonical(b)
	if string(ca) != string(cb) {
		t.Errorf("1 vs 1.0: %s vs %s", ca, cb)
	}
	// Non-integral floats stay distinct from ints.
	c, _ := Canonical(map[string]any{"n": 1.5})
	if string(c) == string(ca) {
		t.Error("1.5 must not equal 1")
	}
}

func TestCanonical_NestedNormalization(t *testing.T) {
	a := map[string]any{"list": []any{1, map[string]any{"k": 2.0}}}
	b := map[string]any{"list": []any{int64(1), map[string]any{"k": 2}}}
	ca, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Errorf("nested normalization: %s vs %s", ca, cb)
	}
}

func TestEqual_PhantomUpdate(t *testing.T) {
	live := map[string]any{"replicas": 1.0, "labels": map[string]any{"a": "1", "b": "2"}}
	want := map[string]any{"replicas": 1, "labels": map[string]any{"b": "2", "a": "1"}}
	if !Equal(live, want) {
		t.Error("canonically equal data must not diff")
	}
	want["replicas"] = 2
	if Equal(live, want) {
		t.Error("changed data must diff")
	}
}

func TestNormalize_UnsupportedType(t *testing.T) {
	if _, err := Normalize(map[string]any{"x": func() {}}); err == nil {
		t.Fatal("expected error for unsupported type")
	}
}
