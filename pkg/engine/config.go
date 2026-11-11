package engine

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DecodeConfig converts a schema config yaml.Node into a generic value with
// ${...} occurrences replaced by Reference or Template markers. Keys are
// plain strings; scalars are string/int/float/bool/nil as yaml gives them.
func DecodeConfig(n *yaml.Node) (any, error) {
	var raw any
	if err := n.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	return markRefs(raw), nil
}

// markRefs walks a decoded value in place-semantics (returns a new tree)
// replacing strings that contain references.
func markRefs(v any) any {
	switch t := v.(type) {
	case string:
		return markString(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = markRefs(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = markRefs(e)
		}
		return out
	default:
		return v
	}
}

// markString classifies one scalar:
//   - plain string with no references: unchanged (with $${ unescaped)
//   - exactly one whole reference: Reference (keeps the value's type)
//   - anything mixing literals and references: Template (result is a string)
func markString(s string) any {
	return buildTemplate(s)
}

// buildTemplate converts s into either a Reference (single whole ref) or a
// Template, or the plain unescaped string.
func buildTemplate(s string) any {
	type piece struct {
		lit string
		ref *Reference
	}
	var pieces []piece
	hasRef := false
	i := 0
	for i < len(s) {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' && (i == 0 || s[i-1] != '$') {
			end := strings.IndexByte(s[i+2:], '}')
			if end == -1 {
				break // unterminated: schema already rejected
			}
			end += i + 2
			inner := s[i+2 : end]
			if r, ok := parseInner(inner); ok {
				hasRef = true
				if len(pieces) > 0 {
					pieces = append(pieces, piece{}) // placeholder fixed below
					pieces[len(pieces)-1] = piece{lit: ""}
				}
				pieces = append(pieces, piece{ref: &r})
				i = end + 1
				continue
			}
		}
		// literal char (handle $${ escape)
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '$' && i+2 < len(s) && s[i+2] == '{' {
			if len(pieces) == 0 || pieces[len(pieces)-1].ref != nil {
				pieces = append(pieces, piece{})
			}
			pieces[len(pieces)-1].lit += "${"
			i += 3
			continue
		}
		if len(pieces) == 0 || pieces[len(pieces)-1].ref != nil {
			pieces = append(pieces, piece{})
		}
		pieces[len(pieces)-1].lit += string(s[i])
		i++
	}
	if !hasRef {
		if un, ok := unescape(s); ok {
			return un
		}
		return s
	}
	// Whole-string single reference?
	if len(pieces) == 1 && pieces[0].ref != nil && pieces[0].lit == "" {
		return *pieces[0].ref
	}
	t := Template{}
	for _, p := range pieces {
		if p.ref != nil {
			t.Parts = append(t.Parts, *p.ref)
		} else if p.lit != "" || len(t.Parts) > 0 {
			t.Parts = append(t.Parts, p.lit)
		}
	}
	if len(t.Parts) == 0 {
		t.Parts = []any{""}
	}
	return t
}

// forEachRef calls fn for every ${...} in s with the inner text and whether
// it is the entire string.
func forEachRef(s string, fn func(inner string, whole bool, start, end int)) {
	whole := isWhole(s)
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) || s[i+1] != '{' {
			continue
		}
		if i > 0 && s[i-1] == '$' {
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end == -1 {
			return
		}
		end += i + 2
		fn(s[i+2:end], whole && i == 0 && end == len(s)-1, i, end)
		i = end
	}
}

func isWhole(s string) bool {
	return len(s) >= 4 && strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") &&
		!strings.Contains(s[2:len(s)-1], "${")
}

// parseInner classifies the inside of ${...}.
func parseInner(inner string) (Reference, bool) {
	if inner == "" {
		return Reference{}, false
	}
	if strings.HasPrefix(inner, "secret.") && len(inner) > 7 {
		return Reference{Reserved: true, Name: inner[7:], Output: "secret"}, true
	}
	if strings.HasPrefix(inner, "env.") && len(inner) > 4 {
		return Reference{Reserved: true, Name: inner[4:], Output: "env"}, true
	}
	node, out, ok := strings.Cut(inner, ".")
	if !ok || node == "" || out == "" || strings.Contains(node, ".") || strings.Contains(out, ".") {
		return Reference{}, false
	}
	for _, r := range node + out {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return Reference{}, false
		}
	}
	return Reference{Node: node, Output: out}, true
}

// unescape replaces $${ with ${. ok is false when nothing changed.
func unescape(s string) (string, bool) {
	if !strings.Contains(s, "$${") {
		return s, false
	}
	out := strings.ReplaceAll(s, "$${", "${")
	return out, true
}

// ResolveOutcome is the result of substituting references in a config.
type ResolveOutcome struct {
	// Value is the fully substituted config; only valid when Known.
	Value any
	// Known is false when any referenced value is Unknown: the whole value
	// and therefore the whole node is poisoned (engine rule 2).
	Known bool
	// UnknownFrom names the node whose pending output poisoned the config.
	UnknownFrom string
}

// Resolve substitutes Reference and Template markers in a marked config
// using lookup. A missing lookup key (node produced no such output) is an
// error; an Unknown value poisons the entire result.
func Resolve(v any, lookup func(node, output string) (Value, bool)) (ResolveOutcome, error) {
	out := ResolveOutcome{Known: true}
	resolved, err := resolveInto(v, lookup, &out)
	if err != nil {
		return ResolveOutcome{}, err
	}
	out.Value = resolved
	return out, nil
}

func resolveInto(v any, lookup func(node, output string) (Value, bool), out *ResolveOutcome) (any, error) {
	switch t := v.(type) {
	case Reference:
		if t.Reserved {
			return nil, fmt.Errorf("${%s.%s} is not implemented until Phase 3", t.Output, t.Name)
		}
		val, ok := lookup(t.Node, t.Output)
		if !ok {
			return nil, fmt.Errorf("node %q has no output %q", t.Node, t.Output)
		}
		if !val.Known {
			out.Known = false
			if out.UnknownFrom == "" {
				out.UnknownFrom = t.Node
			}
			return nil, nil
		}
		return val.Data, nil
	case Template:
		var b strings.Builder
		for _, p := range t.Parts {
			switch part := p.(type) {
			case string:
				b.WriteString(part)
			case Reference:
				val, ok := lookup(part.Node, part.Output)
				if !ok {
					return nil, fmt.Errorf("node %q has no output %q", part.Node, part.Output)
				}
				if !val.Known {
					out.Known = false
					if out.UnknownFrom == "" {
						out.UnknownFrom = part.Node
					}
					return nil, nil
				}
				b.WriteString(fmt.Sprint(val.Data))
			default:
				return nil, fmt.Errorf("internal: bad template part %T", p)
			}
		}
		return b.String(), nil
	case map[string]any:
		res := make(map[string]any, len(t))
		for k, e := range t {
			rv, err := resolveInto(e, lookup, out)
			if err != nil {
				return nil, err
			}
			if !out.Known {
				return nil, nil // poisoned: stop early, whole value unknown
			}
			res[k] = rv
		}
		return res, nil
	case []any:
		res := make([]any, len(t))
		for i, e := range t {
			rv, err := resolveInto(e, lookup, out)
			if err != nil {
				return nil, err
			}
			if !out.Known {
				return nil, nil
			}
			res[i] = rv
		}
		return res, nil
	default:
		return v, nil
	}
}
