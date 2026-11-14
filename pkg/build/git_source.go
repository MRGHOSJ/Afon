// Package build implements Stage 7 build-flow node types: source
// resolution (git-source) and rootless BuildKit image builds that run
// as throwaway Kubernetes Jobs (image-build), plus the Job-running
// primitive they share. The engine never imports this package — node
// types register from cmd/afon — so pkg/engine stays free of
// Kubernetes types (dependency rule: cmd → build → engine).
package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/MRGHOSJ/afon/pkg/engine"
)

// GitSource resolves a git ref to a commit SHA at run time. Plan and
// apply both call git ls-remote (async by contract: a network failure
// degrades to will-run at plan and fails the node at apply), and a
// full 40- or 64-hex ref is already a SHA so it skips the network
// entirely. The resolved SHA rides the cache key, so a moved branch
// or tag invalidates downstream builds automatically.
type GitSource struct{}

// Type implements engine.NodeType.
func (GitSource) Type() string { return "git-source" }

// TypeVersion implements engine.NodeType. Bump when resolution or
// output shape changes.
func (GitSource) TypeVersion() string { return "1" }

// Kind implements engine.NodeType.
func (GitSource) Kind() engine.Kind { return engine.KindAction }

// Validate implements engine.NodeType: {url, ref} as non-empty
// strings; either may be a reference marker resolved later. ref
// rejects whitespace because git refs cannot contain it, and unknown
// fields are refused so typos fail at compile time.
func (GitSource) Validate(cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a mapping")
	}
	for _, key := range []string{"url", "ref"} {
		v, present := m[key]
		if !present {
			return fmt.Errorf("missing required field %q", key)
		}
		s, isStr := v.(string)
		switch {
		case isStr && s == "":
			return fmt.Errorf("%s must not be empty", key)
		case isStr && key == "ref" && strings.ContainsAny(s, " \t\r\n"):
			return fmt.Errorf("ref must not contain whitespace")
		case !isStr && !isRefLike(v):
			return fmt.Errorf("%s must be a string, got %T", key, v)
		}
	}
	for k := range m {
		switch k {
		case "url", "ref":
		default:
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

// DeclaredOutputs implements engine.NodeType: the commit SHA and the
// repository URL it was resolved from (so image-build can reference
// both without re-stating the URL).
func (GitSource) DeclaredOutputs() []string { return []string{"sha", "url"} }

// CacheInputs implements engine.InputCacher: the resolved SHA is the
// external state that must invalidate the key when a ref moves. A
// resolution failure returns the error, which the engine reports as
// will-run (plan) or fails the node (apply) — the designed asymmetry.
func (GitSource) CacheInputs(cfg any, in engine.Inputs) (string, error) {
	m, err := gitSourceConfig(cfg)
	if err != nil {
		return "", err
	}
	sha, err := ResolveRef(context.Background(), m["url"], m["ref"])
	if err != nil {
		return "", err
	}
	return "sha=" + sha, nil
}

// Run implements engine.ActionType: resolve the ref and publish
// sha + url. It re-resolves rather than trusting plan output (plan and
// apply are separate processes); a ref moving between plan and apply
// costs at worst one extra rebuild, never a stale cache hit.
func (GitSource) Run(ctx context.Context, cfg any, in engine.Inputs) (engine.Outputs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := gitSourceConfig(cfg)
	if err != nil {
		return nil, err
	}
	sha, err := ResolveRef(ctx, m["url"], m["ref"])
	if err != nil {
		return nil, err
	}
	return engine.Outputs{
		"sha": engine.KnownValue(sha),
		"url": engine.KnownValue(m["url"]),
	}, nil
}

func gitSourceConfig(cfg any) (map[string]string, error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config must be a mapping")
	}
	out := make(map[string]string, 2)
	for _, key := range []string{"url", "ref"} {
		v, present := m[key]
		if !present {
			return nil, fmt.Errorf("missing required field %q", key)
		}
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("resolved %s must be a non-empty string, got %T", key, v)
		}
		out[key] = s
	}
	return out, nil
}

// ResolveRef turns ref into a commit SHA. A hex ref (40 or 64 hex
// digits) is used as-is; anything else goes through
// `git ls-remote -- <url>` (no patterns: matching happens exactly in
// Go so branch/tag ambiguity is deterministic). Match order: the ref
// verbatim (HEAD or a full refname), refs/heads/<ref>, then
// refs/tags/<ref> preferring the peeled `^{}` line of annotated tags —
// the peeled value is the commit, which is what a build must check
// out. A branch wins over a tag of the same name (clone-like order).
func ResolveRef(ctx context.Context, url, ref string) (string, error) {
	if isHexSHA(ref) {
		return strings.ToLower(ref), nil
	}
	refs, err := gitLSRemote(ctx, url)
	if err != nil {
		return "", err
	}
	if sha, ok := refs[ref]; ok {
		return sha, nil
	}
	if sha, ok := refs["refs/heads/"+ref]; ok {
		return sha, nil
	}
	if sha, ok := refs["refs/tags/"+ref+"^{}"]; ok {
		return sha, nil
	}
	if sha, ok := refs["refs/tags/"+ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("git ls-remote: ref %q not found in %s (use HEAD, a branch, a tag, or a full 40-hex sha)", ref, url)
}

// gitLSRemote runs `git ls-remote -- <url>` and returns refname →
// SHA, including peeled `^{}` lines for annotated tags (ls-remote
// emits them by default when no pattern is given). GIT_TERMINAL_PROMPT=0
// makes a private or wrong URL fail fast instead of hanging on an
// interactive credential prompt; ssh keys still work through an agent.
func gitLSRemote(ctx context.Context, url string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--", url)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			return nil, fmt.Errorf("git not found in PATH: install git or run afon where git is available")
		}
		msg := strings.TrimSpace(string(exitStderr(err)))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git ls-remote %s: %s", url, msg)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		sha, name, ok := strings.Cut(line, "\t")
		if !ok || sha == "" || name == "" {
			continue
		}
		refs[name] = sha
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("git ls-remote %s: no refs (empty repository?)", url)
	}
	return refs, nil
}

// exitStderr pulls a failed command's stderr out of ExitError, which
// cmd.Output captures when Stderr is not otherwise set.
func exitStderr(err error) []byte {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.Stderr
	}
	return nil
}

// isHexSHA reports whether ref is already a full git object id.
func isHexSHA(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for _, r := range ref {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// isRefLike reports whether a config value is a marker the resolver
// substitutes before the node runs (engine.Reference / engine.Template).
func isRefLike(v any) bool {
	switch v.(type) {
	case engine.Reference, engine.Template:
		return true
	}
	return false
}
