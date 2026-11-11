package engine

import (
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/schema"
)

// testRegistry returns the Phase 2 node types, same set the CLI registers.
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, nt := range []NodeType{ImageRef{}, FakeObject{}} {
		if err := reg.Register(nt); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func compile(t *testing.T, src string) (*CompiledFlow, error) {
	t.Helper()
	flow, err := schema.ParseBytes([]byte(src), "flow.yaml")
	if err != nil {
		return nil, err
	}
	return Compile(flow, testRegistry(t))
}

const minimalFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: t
spec:
  nodes:
    - id: image
      type: image-ref
      config: { ref: nginx:1.27 }
`

func TestCompile_Valid(t *testing.T) {
	cf, err := compile(t, minimalFlow)
	if err != nil {
		t.Fatal(err)
	}
	if cf.Name != "t" || len(cf.Nodes) != 1 {
		t.Fatalf("cf = %+v", cf)
	}
	if cf.Nodes[0].Kind != KindAction {
		t.Errorf("kind = %s", cf.Nodes[0].Kind)
	}
	if len(cf.Topo) != 1 || cf.Topo[0] != "image" {
		t.Errorf("topo = %v", cf.Topo)
	}
}

func TestCompile_Errors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSub string
		wantPos string
	}{
		{
			name: "unknown type",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: a
      type: no-such-type
      config: {}
`,
			wantSub: `unknown node type "no-such-type"`,
			wantPos: "flow.yaml:7:13",
		},
		{
			name: "unknown output",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: a
      type: image-ref
      config: {ref: r}
    - id: b
      type: fake-object
      config:
        kind: K
        name: ${a.nope}
`,
			wantSub: `unknown output "nope" for node "a"`,
			wantPos: "flow.yaml:13:15",
		},
		{
			name: "unknown node in reference",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: b
      type: fake-object
      config:
        kind: K
        name: ${ghost.ref}
`,
			wantSub: `references unknown node "ghost"`,
			wantPos: "flow.yaml:10:15",
		},
		{
			name: "needs unknown node",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: b
      type: fake-object
      needs: [ghost]
      config: {kind: K, name: n}
`,
			wantSub: `needs unknown node "ghost"`,
			wantPos: "flow.yaml:8:15",
		},
		{
			name: "cycle",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: a
      type: fake-object
      needs: [b]
      config: {kind: K, name: n}
    - id: b
      type: fake-object
      needs: [a]
      config: {kind: K, name: n}
`,
			wantSub: "cycle:",
			wantPos: "flow.yaml:1:1",
		},
		{
			name: "rollback not implemented",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: a
      type: image-ref
      onFailure: rollback
      config: {ref: r}
`,
			wantSub: "onFailure: rollback not implemented until Phase 4",
			wantPos: "flow.yaml:6:11",
		},
		{
			name: "reserved secret ref",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: a
      type: fake-object
      config:
        kind: K
        name: ${secret.DB}
`,
			wantSub: "${secret.DB} is not implemented until Phase 3",
			wantPos: "flow.yaml:10:15",
		},
		{
			name: "self reference",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: a
      type: fake-object
      config:
        kind: K
        name: ${a.x}
`,
			wantSub: `cannot reference itself`,
			wantPos: "flow.yaml:10:15",
		},
		{
			name: "node config validation",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: t}
spec:
  nodes:
    - id: a
      type: image-ref
      config: {}
`,
			wantSub: `missing required field "ref"`,
			wantPos: "flow.yaml:6:11",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compile(t, tc.src)
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantSub) {
				t.Errorf("message %q must contain %q", msg, tc.wantSub)
			}
			if !strings.HasPrefix(msg, tc.wantPos) {
				t.Errorf("error %q must start with %q", msg, tc.wantPos)
			}
		})
	}
}
