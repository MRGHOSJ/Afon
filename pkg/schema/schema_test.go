package schema

import (
	"strings"
	"testing"
)

const validFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: hello
spec:
  target:
    context: k3d-dev
    namespace: hello
  nodes:
    - id: image
      type: image-ref
      config:
        ref: docker.io/library/nginx:1.27
    - id: obj
      type: fake-object
      needs: [image]
      config:
        kind: ConfigMap
        name: hello
        data:
          digest: ${image.digest}
          label: "sha is ${image.digest}!"
`

func TestParse_Valid(t *testing.T) {
	flow, err := ParseBytes([]byte(validFlow), "flow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if flow.Name != "hello" {
		t.Errorf("name = %q", flow.Name)
	}
	if flow.Target.Context != "k3d-dev" || flow.Target.Namespace != "hello" {
		t.Errorf("target = %+v", flow.Target)
	}
	if len(flow.Nodes) != 2 {
		t.Fatalf("nodes = %d", len(flow.Nodes))
	}
	obj := flow.Nodes[1]
	if obj.ID != "obj" || obj.Type != "fake-object" {
		t.Errorf("node = %+v", obj)
	}
	if len(obj.Needs) != 1 || obj.Needs[0].ID != "image" {
		t.Errorf("needs = %+v", obj.Needs)
	}
	// Two refs in the obj config: whole-string and interpolated.
	if len(flow.Refs) != 2 {
		t.Fatalf("refs = %+v", flow.Refs)
	}
	whole := flow.Refs[0]
	if whole.From != "image" || whole.Output != "digest" || !whole.Whole {
		t.Errorf("whole ref = %+v", whole)
	}
	if whole.Pos.Line != 21 {
		t.Errorf("ref line = %d, want 21", whole.Pos.Line)
	}
	if flow.Refs[1].Whole {
		t.Errorf("interpolated ref must not be whole: %+v", flow.Refs[1])
	}
}

func TestParse_ErrorsHavePositions(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSub string // in the message
		wantPos string // "line:col" prefix check, e.g. "flow.yaml:5:3"
	}{
		{
			name: "wrong apiVersion",
			src: `apiVersion: v1
kind: Flow
metadata: {name: x}
spec:
  nodes:
    - {id: a, type: image-ref, config: {ref: r}}
`,
			wantSub: `unsupported apiVersion "v1"`,
			wantPos: "flow.yaml:1:13",
		},
		{
			name: "wrong kind",
			src: `apiVersion: afon.dev/flow/v1
kind: Pipeline
metadata: {name: x}
spec:
  nodes:
    - {id: a, type: image-ref, config: {ref: r}}
`,
			wantSub: `unsupported kind "Pipeline"`,
			wantPos: "flow.yaml:2:7",
		},
		{
			name: "missing metadata.name",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {}
spec:
  nodes:
    - {id: a, type: image-ref, config: {ref: r}}
`,
			wantSub: "missing metadata.name",
			wantPos: "flow.yaml:3:11",
		},
		{
			name: "empty nodes",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: x}
spec:
  nodes: []
`,
			wantSub: "spec.nodes must be a non-empty list",
			wantPos: "flow.yaml:5:10",
		},
		{
			name: "duplicate id",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: x}
spec:
  nodes:
    - {id: a, type: image-ref, config: {ref: r}}
    - {id: a, type: fake-object, config: {kind: K, name: n}}
`,
			wantSub: `duplicate node id "a"`,
			wantPos: "flow.yaml:7:12",
		},
		{
			name: "invalid id with dot",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: x}
spec:
  nodes:
    - {id: "a.b", type: image-ref, config: {ref: r}}
`,
			wantSub: "invalid node id",
			wantPos: "flow.yaml:6:12",
		},
		{
			name: "missing type",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: x}
spec:
  nodes:
    - {id: a, config: {ref: r}}
`,
			wantSub: `node "a" is missing type`,
			wantPos: "flow.yaml:6:7",
		},
		{
			name: "unterminated reference",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: x}
spec:
  nodes:
    - id: a
      type: fake-object
      config:
        name: ${oops
`,
			wantSub: "unterminated reference",
			wantPos: "flow.yaml:9:15",
		},
		{
			name: "invalid reference shape",
			src: `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: x}
spec:
  nodes:
    - id: a
      type: fake-object
      config:
        name: ${two.dots.here}
`,
			wantSub: "invalid reference",
			wantPos: "flow.yaml:9:15",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.src), "flow.yaml")
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantSub) {
				t.Errorf("message %q must contain %q", msg, tc.wantSub)
			}
			if !strings.HasPrefix(msg, tc.wantPos) {
				t.Errorf("error %q must start with position %q", msg, tc.wantPos)
			}
		})
	}
}

func TestGrammar(t *testing.T) {
	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: g}
spec:
  nodes:
    - id: a
      type: image-ref
      config: {ref: r}
    - id: b
      type: fake-object
      config:
        kind: K
        name: n
        namespace: ${a.ref}
        data:
          whole: ${a.digest}
          interp: "pre-${a.digest}-post"
          escaped: "$${not.a.ref}"
          nested:
            deep: [1, "${a.digest}", "x${a.ref}"]
`
	flow, err := ParseBytes([]byte(src), "flow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var byNode = map[string][]Ref{}
	for _, r := range flow.Refs {
		byNode[r.NodeID] = append(byNode[r.NodeID], r)
	}
	refs := byNode["b"]
	// namespace, whole, interp, and two inside the nested list. The
	// escaped "$${not.a.ref}" must not appear.
	if len(refs) != 5 {
		t.Fatalf("b refs = %d, want 5 (escaped must not count): %+v", len(refs), refs)
	}
	// namespace is a whole-string reference → typed at resolution.
	if !refs[0].Whole || refs[0].Output != "ref" {
		t.Errorf("refs[0] = %+v", refs[0])
	}
	if !refs[1].Whole || refs[1].Output != "digest" {
		t.Errorf("refs[1] = %+v", refs[1])
	}
	if refs[2].Whole {
		t.Errorf("interpolation must not be whole: %+v", refs[2])
	}
	// The nested list: a whole ref and an interpolated one.
	if !refs[3].Whole || refs[3].Output != "digest" {
		t.Errorf("refs[3] = %+v", refs[3])
	}
	if refs[4].Whole || refs[4].Output != "ref" {
		t.Errorf("refs[4] = %+v", refs[4])
	}
	// Positions: escaped line must not appear; check line numbers ascend.
	if refs[0].Pos.Line != 14 || refs[1].Pos.Line != 16 || refs[2].Pos.Line != 17 {
		t.Errorf("positions wrong: %+v", refs)
	}
	if refs[3].Pos.Line != 20 || refs[4].Pos.Line != 20 {
		t.Errorf("nested ref lines wrong: %+v", refs[3:])
	}
}

func TestReservedRefs_Parse(t *testing.T) {
	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: g}
spec:
  nodes:
    - id: a
      type: fake-object
      config:
        kind: K
        name: ${secret.DB_PASSWORD}
`
	flow, err := ParseBytes([]byte(src), "flow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(flow.Refs) != 1 {
		t.Fatalf("refs = %+v", flow.Refs)
	}
	r := flow.Refs[0]
	if !r.Reserved || r.Output != "secret" || r.From != "DB_PASSWORD" {
		t.Errorf("reserved ref = %+v", r)
	}
}

func TestNeeds_Positions(t *testing.T) {
	src := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: g}
spec:
  nodes:
    - id: a
      type: image-ref
      config: {ref: r}
    - id: b
      type: fake-object
      needs: [ghost]
      config: {kind: K, name: n}
`
	flow, err := ParseBytes([]byte(src), "flow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	needs := flow.Nodes[1].Needs
	if len(needs) != 1 || needs[0].ID != "ghost" {
		t.Fatalf("needs = %+v", needs)
	}
	if needs[0].Pos.Line != 11 {
		t.Errorf("needs pos = %s", needs[0].Pos)
	}
}
