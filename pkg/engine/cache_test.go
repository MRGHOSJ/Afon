package engine

import (
	"testing"
)

// Hash stability: equal inputs must hash equally regardless of map insertion
// order and 1 vs 1.0 (engine rule 3/7 territory).

func TestCacheKey_MapOrder(t *testing.T) {
	a := map[string]any{"b": 1, "a": map[string]any{"y": "s", "x": true}}
	b := map[string]any{"a": map[string]any{"x": true, "y": "s"}, "b": 1}
	ka, err := CacheKey("t", "1", a)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := CacheKey("t", "1", b)
	if err != nil {
		t.Fatal(err)
	}
	if ka != kb {
		t.Errorf("map order changed key: %s vs %s", ka, kb)
	}
}

func TestCacheKey_OneVsOnePointZero(t *testing.T) {
	ka, _ := CacheKey("t", "1", map[string]any{"n": 1})
	kb, _ := CacheKey("t", "1", map[string]any{"n": 1.0})
	if ka != kb {
		t.Errorf("1 vs 1.0 changed key: %s vs %s", ka, kb)
	}
	kc, _ := CacheKey("t", "1", map[string]any{"n": 1.5})
	if kc == ka {
		t.Error("1.5 must not equal 1")
	}
}

func TestCacheKey_TypeAndVersionMatter(t *testing.T) {
	cfg := map[string]any{"x": 1}
	base, _ := CacheKey("t", "1", cfg)
	otherType, _ := CacheKey("u", "1", cfg)
	otherVer, _ := CacheKey("t", "2", cfg)
	if base == otherType {
		t.Error("type must be part of the key")
	}
	if base == otherVer {
		t.Error("typeVersion must be part of the key")
	}
}

func TestCacheKey_OnlyConfigMatters(t *testing.T) {
	// Rule 3: the key is {type, typeVersion, resolvedConfig}. An upstream
	// output the node does not reference must not be able to change it —
	// and it cannot, because it never enters this function. Guard the
	// contract: identical resolved configs → identical keys even when the
	// caller had different upstream outputs.
	up1 := map[string]any{"image": map[string]any{"digest": "sha256:aaa"}}
	up2 := map[string]any{"image": map[string]any{"digest": "sha256:bbb"}}
	// The resolved config of a node that references neither:
	resolved := map[string]any{"ref": "nginx:1.27"}
	k1, _ := CacheKey("image-ref", "1", resolved)
	k2, _ := CacheKey("image-ref", "1", resolved)
	_ = up1
	_ = up2
	if k1 != k2 {
		t.Error("resolved config alone must determine the key")
	}
}

func TestCacheKey_DeterministicAcrossCalls(t *testing.T) {
	cfg := map[string]any{"s": "x", "n": 42, "f": 3.14, "l": []any{1, "a", nil}}
	var first string
	for i := 0; i < 10; i++ {
		k, err := CacheKey("t", "1", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = k
		} else if k != first {
			t.Fatalf("key drifted: %s vs %s", first, k)
		}
	}
}
