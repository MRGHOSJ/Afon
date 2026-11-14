package build

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MRGHOSJ/afon/pkg/engine"
)

// requireGit skips a test when git is unavailable (hermetic unit rule:
// fixtures shell out to the real git, never the network).
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
}

// runGit runs a git subcommand inside dir and returns stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fileURL converts a local path to a file:// URL that works on
// Windows (C:/...) and Unix ( /...).
func fileURL(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// initFixtureRepo builds a one-commit repo with main as HEAD, one
// lightweight tag (v0.1), and one annotated tag (v1.0). Returns the
// directory and the commit SHA.
func initFixtureRepo(t *testing.T) (string, string) {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "afon-test@example.invalid")
	runGit(t, dir, "config", "user.name", "Afon Test")
	if err := writeFile(filepath.Join(dir, "hello.txt"), "hello\n"); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "hello.txt")
	runGit(t, dir, "commit", "-m", "init")
	sha := runGit(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "tag", "v0.1")
	runGit(t, dir, "tag", "-a", "v1.0", "-m", "annotated")
	return dir, sha
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func resolve(t *testing.T, url, ref string) string {
	t.Helper()
	sha, err := ResolveRef(context.Background(), url, ref)
	if err != nil {
		t.Fatalf("ResolveRef(%q): %v", ref, err)
	}
	return sha
}

func TestGitSource_Meta(t *testing.T) {
	var n GitSource
	if got := n.Type(); got != "git-source" {
		t.Errorf("Type = %q", got)
	}
	if got := n.TypeVersion(); got != "1" {
		t.Errorf("TypeVersion = %q", got)
	}
	if n.Kind() != engine.KindAction {
		t.Errorf("Kind = %q", n.Kind())
	}
	want := []string{"sha", "url"}
	if got := n.DeclaredOutputs(); !reflect.DeepEqual(got, want) {
		t.Errorf("DeclaredOutputs = %v, want %v", got, want)
	}
}

func TestGitSource_Branch(t *testing.T) {
	dir, sha := initFixtureRepo(t)
	got := resolve(t, fileURL(dir), "main")
	if got != sha {
		t.Errorf("branch main = %s, want %s", got, sha)
	}
}

func TestGitSource_HEAD(t *testing.T) {
	dir, sha := initFixtureRepo(t)
	if got := resolve(t, fileURL(dir), "HEAD"); got != sha {
		t.Errorf("HEAD = %s, want %s", got, sha)
	}
}

func TestGitSource_FullRefname(t *testing.T) {
	dir, sha := initFixtureRepo(t)
	if got := resolve(t, fileURL(dir), "refs/heads/main"); got != sha {
		t.Errorf("refs/heads/main = %s, want %s", got, sha)
	}
}

func TestGitSource_LightweightTag(t *testing.T) {
	dir, sha := initFixtureRepo(t)
	if got := resolve(t, fileURL(dir), "v0.1"); got != sha {
		t.Errorf("tag v0.1 = %s, want %s", got, sha)
	}
}

func TestGitSource_AnnotatedTagPeeled(t *testing.T) {
	dir, sha := initFixtureRepo(t)
	tagObj := runGit(t, dir, "rev-parse", "v1.0")
	if c := runGit(t, dir, "rev-parse", "v1.0^{commit}"); c != sha {
		t.Fatalf("fixture broken: peeled commit %s != HEAD %s", c, sha)
	}
	if tagObj == sha {
		t.Fatal("fixture broken: annotated tag object equals commit")
	}
	got := resolve(t, fileURL(dir), "v1.0")
	if got != sha {
		t.Errorf("annotated tag v1.0 = %s, want commit %s (peeled), not tag object %s", got, sha, tagObj)
	}
}

func TestGitSource_BranchWinsOverTag(t *testing.T) {
	dir, first := initFixtureRepo(t)
	runGit(t, dir, "tag", "amb")
	if err := writeFile(filepath.Join(dir, "second.txt"), "second\n"); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "second.txt")
	runGit(t, dir, "commit", "-m", "second")
	second := runGit(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "branch", "-f", "amb")
	if first == second {
		t.Fatal("fixture broken: commits equal")
	}
	got := resolve(t, fileURL(dir), "amb")
	if got != second {
		t.Errorf("ambiguous ref amb = %s, want branch %s (heads win over tag %s)", got, second, first)
	}
}

func TestGitSource_HexSkipsNetwork(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	got, err := ResolveRef(context.Background(), "file:///definitely/not/a/repo", sha)
	if err != nil {
		t.Fatalf("hex ref must not run git: %v", err)
	}
	if got != sha {
		t.Errorf("hex ref = %s, want %s", got, sha)
	}
	upper, err := ResolveRef(context.Background(), "file:///definitely/not/a/repo", strings.ToUpper(sha))
	if err != nil {
		t.Fatalf("uppercase hex ref: %v", err)
	}
	if upper != sha {
		t.Errorf("uppercase hex normalized to %s, want %s", upper, sha)
	}
	long := strings.Repeat("ab", 32)
	got, err = ResolveRef(context.Background(), "file:///definitely/not/a/repo", long)
	if err != nil || got != long {
		t.Errorf("64-hex ref: got (%q, %v), want (%q, nil)", got, err, long)
	}
}

func TestGitSource_MissingRef(t *testing.T) {
	dir, _ := initFixtureRepo(t)
	url := fileURL(dir)
	_, err := ResolveRef(context.Background(), url, "no-such-ref")
	if err == nil {
		t.Fatal("expected error for unknown ref")
	}
	if !strings.Contains(err.Error(), "no-such-ref") || !strings.Contains(err.Error(), url) {
		t.Errorf("error should name ref and url, got: %v", err)
	}
}

func TestGitSource_NoGitInPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	n := GitSource{}
	cfg := map[string]any{"url": "file:///unused", "ref": "main"}
	if _, err := n.Run(context.Background(), cfg, engine.Inputs{}); err == nil || !strings.Contains(err.Error(), "git not found") {
		t.Errorf("Run without git: %v, want git not found", err)
	}
	if _, err := n.CacheInputs(cfg, engine.Inputs{}); err == nil || !strings.Contains(err.Error(), "git not found") {
		t.Errorf("CacheInputs without git: %v, want git not found", err)
	}
}

func TestGitSource_MovedBranchChangesCacheKey(t *testing.T) {
	dir, first := initFixtureRepo(t)
	n := GitSource{}
	cfg := map[string]any{"url": fileURL(dir), "ref": "main"}
	key1, err := n.CacheInputs(cfg, engine.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha=" + first; key1 != want {
		t.Fatalf("CacheInputs = %q, want %q", key1, want)
	}
	if err := writeFile(filepath.Join(dir, "more.txt"), "more\n"); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "more.txt")
	runGit(t, dir, "commit", "-m", "more")
	second := runGit(t, dir, "rev-parse", "HEAD")
	key2, err := n.CacheInputs(cfg, engine.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if key1 == key2 {
		t.Errorf("cache key unchanged after branch moved: %q", key1)
	}
	if want := "sha=" + second; key2 != want {
		t.Errorf("CacheInputs after move = %q, want %q", key2, want)
	}
}

func TestGitSource_RunOutputs(t *testing.T) {
	dir, sha := initFixtureRepo(t)
	url := fileURL(dir)
	n := GitSource{}
	outs, err := n.Run(context.Background(), map[string]any{"url": url, "ref": "main"}, engine.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := outs["sha"]; !ok || !got.Known || got.Data != sha {
		t.Errorf("sha output = %#v, want %s", outs["sha"], sha)
	}
	if got, ok := outs["url"]; !ok || !got.Known || got.Data != url {
		t.Errorf("url output = %#v, want %s", outs["url"], url)
	}
}

func TestGitSource_RunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := GitSource{}.Run(ctx, map[string]any{"url": "file:///unused", "ref": "main"}, engine.Inputs{})
	if err == nil {
		t.Fatal("expected cancelled context error")
	}
}

func TestGitSource_Validate(t *testing.T) {
	valid := map[string]any{"url": "https://example.invalid/r.git", "ref": "main"}
	refMarker := map[string]any{"url": engine.Reference{}, "ref": engine.Template{}}
	cases := []struct {
		name    string
		cfg     any
		wantErr string
	}{
		{"valid", valid, ""},
		{"markers accepted", refMarker, ""},
		{"not a mapping", "nope", "must be a mapping"},
		{"missing url", map[string]any{"ref": "main"}, `missing required field "url"`},
		{"empty url", map[string]any{"url": "", "ref": "main"}, "url must not be empty"},
		{"missing ref", map[string]any{"url": "u"}, `missing required field "ref"`},
		{"empty ref", map[string]any{"url": "u", "ref": ""}, "ref must not be empty"},
		{"whitespace ref", map[string]any{"url": "u", "ref": "my branch"}, "whitespace"},
		{"non-string url", map[string]any{"url": 42, "ref": "main"}, "url must be a string"},
		{"unknown field", map[string]any{"url": "u", "ref": "main", "branch": "x"}, `unknown field "branch"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GitSource{}.Validate(tc.cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("want error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
