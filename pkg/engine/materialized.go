package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// materializedOutputs supplies a materialized action node's outputs
// from its file: every declared output set to the file's manifest body
// (the provenance header is stripped — it is provenance, not content —
// so render-time chains and plan/apply see identical bytes). Returns
// ok=false when the node does not materialize (not a MaterializedNode,
// or out: unset), which keeps the ephemeral path untouched. A missing
// file is an error naming the node and path regardless of whether
// anything references the node: the file is the artifact, and plan must
// not silently fall back to cache or will-run when it is gone.
func materializedOutputs(cn *CompiledNode, in Inputs) (outs Outputs, path string, ok bool, err error) {
	path, err = materializedPath(cn, in)
	if err != nil {
		return nil, "", false, fmt.Errorf("node %q: %w", cn.ID, err)
	}
	if path == "" {
		return nil, "", false, nil
	}
	b, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", false, fmt.Errorf(
				"materialized output for node %q is missing at %s; run \"afon render\" first",
				cn.ID, path)
		}
		return nil, "", false, fmt.Errorf("node %q: reading materialized output at %s: %w", cn.ID, path, err)
	}
	// A file with a valid header contributes its body; a headerless file
	// (hand-created, or header stripped by an editor) contributes its
	// whole content — it still parses as YAML either way.
	body := b
	if _, parsed, hasHeader := parseProvenance(b); hasHeader {
		body = parsed
	}
	outs = Outputs{}
	for _, name := range cn.NT.DeclaredOutputs() {
		outs[name] = KnownValue(string(body))
	}
	return outs, path, true, nil
}

// materializedPath returns the slash-normalized file path a
// materialized node writes to ("" when the node does not materialize —
// not a MaterializedNode, or out: unset). The path comes from the
// node's marked config: out is required to be a literal, so it never
// depends on resolution state.
func materializedPath(cn *CompiledNode, in Inputs) (string, error) {
	mn, isMaterialized := cn.NT.(MaterializedNode)
	if !isMaterialized {
		return "", nil
	}
	path, err := mn.MaterializedPath(cn.Config, in)
	if err != nil {
		return "", err
	}
	return path, nil
}
