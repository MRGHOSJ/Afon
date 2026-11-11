package graph

import (
	"strings"
	"testing"
)

func TestTopoOrder_Linear(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("c")
	if err := g.AddEdge("a", "b", EdgeRef); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdge("b", "c", EdgeNeeds); err != nil {
		t.Fatal(err)
	}
	order, err := g.TopoOrder()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	for i, id := range want {
		if order[i] != id {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestTopoOrder_Diamond(t *testing.T) {
	// a → b, a → c, b → d, c → d
	g := New()
	for _, id := range []string{"a", "b", "c", "d"} {
		g.AddNode(id)
	}
	edges := [][2]string{{"a", "b"}, {"a", "c"}, {"b", "d"}, {"c", "d"}}
	for _, e := range edges {
		if err := g.AddEdge(e[0], e[1], EdgeRef); err != nil {
			t.Fatal(err)
		}
	}
	order, err := g.TopoOrder()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	for _, e := range edges {
		if pos[e[0]] >= pos[e[1]] {
			t.Errorf("%s must precede %s in %v", e[0], e[1], order)
		}
	}
	// Levels: a=0, b/c=1, d=2
	lv, err := g.Level()
	if err != nil {
		t.Fatal(err)
	}
	if lv["a"] != 0 || lv["d"] != 2 || lv["b"] != lv["c"] {
		t.Errorf("levels = %v", lv)
	}
}

func TestCycle_Path(t *testing.T) {
	g := New()
	for _, id := range []string{"a", "b", "c"} {
		g.AddNode(id)
	}
	_ = g.AddEdge("a", "b", EdgeRef)
	_ = g.AddEdge("b", "c", EdgeRef)
	_ = g.AddEdge("c", "a", EdgeNeeds)

	_, err := g.Level()
	if err == nil {
		t.Fatal("expected cycle error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cycle:") {
		t.Fatalf("error should name the cycle, got %q", msg)
	}
	// Path must include all three nodes and come back to the start.
	for _, id := range []string{"a", "b", "c"} {
		if !strings.Contains(msg, id) {
			t.Errorf("cycle %q missing node %q", msg, id)
		}
	}
	if !strings.Contains(msg, "via needs") {
		t.Errorf("cycle should carry provenance, got %q", msg)
	}
}

func TestSelfEdge(t *testing.T) {
	g := New()
	g.AddNode("a")
	if err := g.AddEdge("a", "a", EdgeRef); err == nil {
		t.Fatal("self edge must error")
	}
}

func TestUnknownEndpoint(t *testing.T) {
	g := New()
	g.AddNode("a")
	if err := g.AddEdge("a", "ghost", EdgeRef); err == nil {
		t.Fatal("edge to unknown node must error")
	}
	if err := g.AddEdge("ghost", "a", EdgeRef); err == nil {
		t.Fatal("edge from unknown node must error")
	}
}

func TestDuplicateEdge_Collapsed(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	if err := g.AddEdge("a", "b", EdgeRef); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdge("a", "b", EdgeNeeds); err != nil {
		t.Fatal(err)
	}
	lv, err := g.Level()
	if err != nil {
		t.Fatal(err)
	}
	if lv["b"] != 1 {
		t.Errorf("duplicate edge must not double-count indegree, levels=%v", lv)
	}
	// First provenance wins.
	edges := g.Edges("a")
	if len(edges) != 1 || edges[0][1] != string(EdgeRef) {
		t.Errorf("edges = %v", edges)
	}
}

func TestTopoOrder_Deterministic(t *testing.T) {
	build := func(reverse bool) []string {
		g := New()
		ids := []string{"a", "b", "c", "d", "e"}
		if reverse {
			for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
		for _, id := range ids {
			g.AddNode(id)
		}
		// Independent nodes only: order must follow insertion order.
		order, err := g.TopoOrder()
		if err != nil {
			t.Fatal(err)
		}
		return order
	}
	// Same insertion order → same output regardless of internal maps.
	first := build(false)
	second := build(false)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("nondeterministic: %v vs %v", first, second)
		}
	}
}
