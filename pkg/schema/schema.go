// Package schema parses and structurally validates flow files. It keeps
// source positions on everything it returns so higher layers (engine.Compile)
// can report errors as flow.yaml:12:9. It knows nothing about node types,
// declared outputs, or cycles: those need the registry and the graph and
// belong to Compile (design rule 5).
package schema

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// APIVersion and Kind are the only accepted document identity.
const (
	APIVersion = "afon.dev/flow/v1"
	KindFlow   = "Flow"
)

// Position locates a value in the source file.
type Position struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

func (p Position) String() string {
	if p.File == "" {
		return fmt.Sprintf("%d:%d", p.Line, p.Column)
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Column)
}

// Error is a validation failure tied to a source position. Every error this
// package returns is an *Error so callers can print file:line:col.
type Error struct {
	Pos     Position
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Message)
}

// Node is one flow node with the positions of its fields.
type Node struct {
	ID   string
	Type string
	// Needs holds explicit ordering dependencies with the position of each
	// entry in the source list.
	Needs []NeedsEntry
	// Config is the raw config subtree; references inside it were extracted
	// into Refs but the tree itself is kept untouched for the node type.
	Config    yaml.Node
	OnFailure string

	IDPos   Position
	TypePos Position
}

// NeedsEntry is one element of `needs:`.
type NeedsEntry struct {
	ID  string
	Pos Position
}

// Ref is one `${...}` occurrence found in a node's config.
type Ref struct {
	// From is the node the reference points at. For reserved forms it holds
	// the name after the prefix instead (Output is "secret" or "env").
	From string
	// Output is the output name; "secret"/"env" name for the reserved forms.
	Output string
	// Reserved is true for ${secret.NAME} and ${env.NAME}.
	Reserved bool
	// Whole is true when the reference is the entire scalar, which preserves
	// the referenced value's type instead of string interpolation.
	Whole bool
	// Pos is the position of the scalar containing the reference (or of the
	// reference itself when it sits alone).
	Pos Position
	// NodeID is the owning node the reference was found in.
	NodeID string
}

// Flow is a parsed, structurally valid flow file.
type Flow struct {
	Name   string
	Target Target
	Nodes  []Node
	// Refs are all references found across all nodes, in document order.
	Refs []Ref
	// Source is the raw file content, kept for hashing FlowHash later.
	Source []byte
	// File is the path the flow was read from.
	File string
}

// Target is spec.target.
type Target struct {
	Context   string
	Namespace string
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Parse decodes a flow file from disk.
func Parse(path string) (*Flow, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	flow, err := ParseBytes(src, path)
	if err != nil {
		return nil, err
	}
	flow.Source = src
	return flow, nil
}

// ParseBytes decodes a flow document. file is used in error positions.
func ParseBytes(src []byte, file string) (*Flow, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		// yaml.TypeError and syntax errors already carry line info; wrap to
		// attach the filename.
		return nil, &Error{
			Pos:     Position{File: file, Line: lineOfYAMLError(err), Column: 1},
			Message: cleanYAMLError(err),
		}
	}
	if doc.Kind == 0 {
		return nil, &Error{Pos: Position{File: file, Line: 1, Column: 1}, Message: "empty document"}
	}

	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, &Error{Pos: Position{File: file, Line: 1, Column: 1}, Message: "empty document"}
		}
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, &Error{
			Pos:     posOf(file, root),
			Message: fmt.Sprintf("flow must be a mapping, got %s", kindName(root)),
		}
	}

	flow := &Flow{File: file}
	m := mapping(root)

	if err := requireScalar(m, "apiVersion", file, APIVersion); err != nil {
		return nil, err
	}
	if v := scalarString(m["apiVersion"]); v != APIVersion {
		return nil, &Error{
			Pos:     posOf(file, m["apiVersion"]),
			Message: fmt.Sprintf("unsupported apiVersion %q, want %q", v, APIVersion),
		}
	}
	if err := requireScalar(m, "kind", file, KindFlow); err != nil {
		return nil, err
	}
	if v := scalarString(m["kind"]); v != KindFlow {
		return nil, &Error{
			Pos:     posOf(file, m["kind"]),
			Message: fmt.Sprintf("unsupported kind %q, want %q", v, KindFlow),
		}
	}

	metaNode, ok := m["metadata"]
	if !ok {
		return nil, &Error{Pos: Position{File: file, Line: 1, Column: 1}, Message: "missing metadata"}
	}
	meta := mapping(metaNode)
	nameNode, ok := meta["name"]
	if !ok {
		return nil, &Error{Pos: posOf(file, metaNode), Message: "missing metadata.name"}
	}
	name := scalarString(nameNode)
	if name == "" {
		return nil, &Error{Pos: posOf(file, nameNode), Message: "metadata.name must not be empty"}
	}
	flow.Name = name

	specNode, ok := m["spec"]
	if !ok {
		return nil, &Error{Pos: posOf(file, root), Message: "missing spec"}
	}
	spec := mapping(specNode)

	if tNode, ok := spec["target"]; ok {
		t := mapping(tNode)
		if c, ok := t["context"]; ok {
			flow.Target.Context = scalarString(c)
		}
		if n, ok := t["namespace"]; ok {
			flow.Target.Namespace = scalarString(n)
		}
	}

	nodesNode, ok := spec["nodes"]
	if !ok || nodesNode.Kind != yaml.SequenceNode || len(nodesNode.Content) == 0 {
		pos := posOf(file, specNode)
		if ok {
			pos = posOf(file, nodesNode)
		}
		return nil, &Error{Pos: pos, Message: "spec.nodes must be a non-empty list"}
	}

	seen := map[string]Position{}
	for _, item := range nodesNode.Content {
		node, refs, err := parseNode(item, file)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[node.ID]; dup {
			return nil, &Error{
				Pos:     node.IDPos,
				Message: fmt.Sprintf("duplicate node id %q (first defined at %s)", node.ID, prev),
			}
		}
		seen[node.ID] = node.IDPos
		flow.Nodes = append(flow.Nodes, *node)
		flow.Refs = append(flow.Refs, refs...)
	}
	return flow, nil
}

func parseNode(item *yaml.Node, file string) (*Node, []Ref, error) {
	if item.Kind != yaml.MappingNode {
		return nil, nil, &Error{Pos: posOf(file, item), Message: fmt.Sprintf("node must be a mapping, got %s", kindName(item))}
	}
	m := mapping(item)

	idNode, ok := m["id"]
	if !ok {
		return nil, nil, &Error{Pos: posOf(file, item), Message: "node is missing id"}
	}
	id := scalarString(idNode)
	if id == "" {
		return nil, nil, &Error{Pos: posOf(file, idNode), Message: "node id must not be empty"}
	}
	if !idPattern.MatchString(id) {
		return nil, nil, &Error{
			Pos:     posOf(file, idNode),
			Message: fmt.Sprintf("invalid node id %q: only letters, digits, '-' and '_' are allowed", id),
		}
	}

	typeNode, ok := m["type"]
	if !ok {
		return nil, nil, &Error{Pos: posOf(file, item), Message: fmt.Sprintf("node %q is missing type", id)}
	}
	nodeType := scalarString(typeNode)
	if nodeType == "" {
		return nil, nil, &Error{Pos: posOf(file, typeNode), Message: fmt.Sprintf("node %q has an empty type", id)}
	}

	n := &Node{
		ID:      id,
		Type:    nodeType,
		IDPos:   posOf(file, idNode),
		TypePos: posOf(file, typeNode),
	}

	if needsNode, ok := m["needs"]; ok {
		if needsNode.Kind != yaml.SequenceNode {
			return nil, nil, &Error{Pos: posOf(file, needsNode), Message: fmt.Sprintf("node %q: needs must be a list", id)}
		}
		for _, e := range needsNode.Content {
			dep := scalarString(e)
			if dep == "" {
				return nil, nil, &Error{Pos: posOf(file, e), Message: fmt.Sprintf("node %q: needs entries must not be empty", id)}
			}
			if !idPattern.MatchString(dep) {
				return nil, nil, &Error{
					Pos:     posOf(file, e),
					Message: fmt.Sprintf("node %q: invalid needs entry %q", id, dep),
				}
			}
			n.Needs = append(n.Needs, NeedsEntry{ID: dep, Pos: posOf(file, e)})
		}
	}

	if ofNode, ok := m["onFailure"]; ok {
		of := scalarString(ofNode)
		switch of {
		case "halt", "rollback":
			n.OnFailure = of
		default:
			return nil, nil, &Error{
				Pos:     posOf(file, ofNode),
				Message: fmt.Sprintf("node %q: unknown onFailure %q (want halt or rollback)", id, of),
			}
		}
	}

	if cfgNode, ok := m["config"]; ok {
		n.Config = *cfgNode
		refs, err := extractRefs(cfgNode, id, file)
		if err != nil {
			return nil, nil, err
		}
		return n, refs, nil
	}
	n.Config = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
	return n, nil, nil
}

// extractRefs walks a config subtree in document order and records every
// ${...} occurrence.
func extractRefs(cfg *yaml.Node, nodeID, file string) ([]Ref, error) {
	var out []Ref
	var walkErr error
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		switch n.Kind {
		case yaml.ScalarNode:
			if n.Tag == "!!str" || n.Tag == "" {
				wholeHere := isWholeRef(n.Value)
				refs, err := scanStringRefs(n.Value, nodeID, posOf(file, n), wholeHere)
				if err != nil {
					// Attach position and abort via panic-free channel: we
					// return by storing the first error.
					if walkErr == nil {
						walkErr = err
					}
					return
				}
				out = append(out, refs...)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(n.Content[i])
				walk(n.Content[i+1])
			}
		case yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c)
			}
		}
	}
	walk(cfg)
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// isWholeRef reports whether s is exactly one reference, e.g. "${a.b}".
func isWholeRef(s string) bool {
	if len(s) < 4 || !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return false
	}
	// "${a.b}${c.d}" is not whole; a single ref has no unescaped "${" after
	// the first two characters.
	inner := s[2 : len(s)-1]
	return !containsRefStart(inner)
}

func containsRefStart(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '$' && s[i+1] == '{' {
			// $${ is an escape, not a real start.
			if i > 0 && s[i-1] == '$' {
				continue
			}
			return true
		}
	}
	return false
}

// scanStringRefs finds every ${...} in s. Escaped $${ produces nothing.
func scanStringRefs(s, nodeID string, pos Position, whole bool) ([]Ref, error) {
	var out []Ref
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) || s[i+1] != '{' {
			continue
		}
		if i > 0 && s[i-1] == '$' {
			// Escaped: $${ — the second dollar starts nothing.
			continue
		}
		end := -1
		for j := i + 2; j < len(s); j++ {
			if s[j] == '}' {
				end = j
				break
			}
		}
		if end == -1 {
			return nil, &Error{
				Pos:     pos,
				Message: fmt.Sprintf("unterminated reference starting at ${%s", s[i+2:min(len(s), i+12)]),
			}
		}
		inner := s[i+2 : end]
		ref, ok := parseRefInner(inner, nodeID, pos, whole)
		if !ok {
			return nil, &Error{
				Pos:     pos,
				Message: fmt.Sprintf("invalid reference ${%s}: want ${node.output}, ${secret.NAME}, or ${env.NAME}", inner),
			}
		}
		out = append(out, ref)
		i = end
	}
	return out, nil
}

// parseRefInner classifies the inside of ${...}.
func parseRefInner(inner, nodeID string, pos Position, whole bool) (Ref, bool) {
	if inner == "" {
		return Ref{}, false
	}
	// Reserved forms: secret.NAME / env.NAME (NAME may not contain dots).
	if len(inner) > 7 && inner[:7] == "secret." {
		return Ref{Output: "secret", From: inner[7:], Reserved: true, Whole: whole, Pos: pos, NodeID: nodeID}, true
	}
	if len(inner) > 4 && inner[:4] == "env." {
		return Ref{Output: "env", From: inner[4:], Reserved: true, Whole: whole, Pos: pos, NodeID: nodeID}, true
	}
	// Regular: nodeID.outputName, exactly one dot, neither side empty,
	// neither side containing a dot.
	dot := -1
	for i := 0; i < len(inner); i++ {
		if inner[i] == '.' {
			if dot != -1 {
				return Ref{}, false // two dots: not a valid reference
			}
			dot = i
		}
	}
	if dot <= 0 || dot == len(inner)-1 {
		return Ref{}, false
	}
	from, output := inner[:dot], inner[dot+1:]
	if !idPattern.MatchString(from) || !idPattern.MatchString(output) {
		return Ref{}, false
	}
	return Ref{From: from, Output: output, Whole: whole, Pos: pos, NodeID: nodeID}, true
}

// mapping indexes a mapping node's children by key. Later duplicates win,
// matching how the scalar lookups behave; structural dupes are not our job.
func mapping(n *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	if n == nil || n.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if k.Kind == yaml.ScalarNode {
			out[k.Value] = n.Content[i+1]
		}
	}
	return out
}

func requireScalar(m map[string]*yaml.Node, key, file, want string) error {
	n, ok := m[key]
	if !ok {
		return &Error{Pos: Position{File: file, Line: 1, Column: 1}, Message: "missing " + key}
	}
	if n.Kind != yaml.ScalarNode {
		return &Error{Pos: posOf(file, n), Message: fmt.Sprintf("%s must be a string", key)}
	}
	return nil
}

func scalarString(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func posOf(file string, n *yaml.Node) Position {
	if n == nil {
		return Position{File: file, Line: 1, Column: 1}
	}
	return Position{File: file, Line: n.Line, Column: n.Column}
}

func kindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "nothing"
	}
}

// lineOfYAMLError best-effort extracts a line number from yaml's error text.
func lineOfYAMLError(err error) int {
	var te *yaml.TypeError
	if ok := asTypeError(err, &te); ok && te != nil && len(te.Errors) > 0 {
		return yamlErrorLine(te.Errors[0])
	}
	return yamlErrorLine(err.Error())
}

func yamlErrorLine(s string) int {
	// yaml errors look like "yaml: line 3: ..."
	for i := 0; i+5 < len(s); i++ {
		if s[i:i+5] == "line " {
			n := 0
			for j := i + 5; j < len(s) && s[j] >= '0' && s[j] <= '9'; j++ {
				n = n*10 + int(s[j]-'0')
			}
			if n > 0 {
				return n
			}
		}
	}
	return 1
}

func cleanYAMLError(err error) string {
	msg := err.Error()
	if len(msg) > 5 && msg[:5] == "yaml:" {
		return msg[6:] // drop "yaml: " prefix; position is in Pos
	}
	return msg
}

func asTypeError(err error, out **yaml.TypeError) bool {
	te, ok := err.(*yaml.TypeError)
	if ok {
		*out = te
	}
	return ok
}
