// Package tests drives the built afon binary end to end. Two scopes:
// error paths (every way the CLI can exit non-zero with an actionable
// message) and the materialized render → plan → apply lifecycle
// (render_test.go). Golden success output for other commands lives in
// cmd/afon.
package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var afonBin string

// TestMain builds the binary once for the whole package; cmd.Dir points at
// the module root so the build sees go.mod.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "afon-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}
	name := "afon"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	afonBin = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", afonBin, "./cmd/afon")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build failed: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fixtureKubeconfig mirrors the unit-test fixture: one context, "alpha".
const fixtureKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c1
  cluster:
    server: https://127.0.0.1:6443
users:
- name: u1
  user: {}
contexts:
- name: alpha
  context:
    cluster: c1
    user: u1
current-context: alpha
`

func writeKubeconfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type result struct {
	stdout string
	stderr string
	code   int
}

// runAf executes the binary with env overrides applied on top of the
// current environment (existing KUBECONFIG entries are stripped first so an
// override always wins).
func runAf(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, afonBin, args...)
	var filtered []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KUBECONFIG=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	for k, v := range env {
		filtered = append(filtered, k+"="+v)
	}
	cmd.Env = filtered

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("afon %v timed out after 60s", args)
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("afon %v failed to run: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func wantExit(t *testing.T, r result, args ...string) {
	t.Helper()
	if r.code != 1 {
		t.Errorf("afon %v exit = %d, want 1\nstderr: %s", args, r.code, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("afon %v stdout = %q, want empty on error", args, r.stdout)
	}
}

func wantStderrContains(t *testing.T, r result, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(r.stderr, w) {
			t.Errorf("afon stderr missing %q\ngot: %s", w, r.stderr)
		}
	}
}

// wantStderrExact asserts the full stderr byte-for-byte, trailing newline
// included. The three tests using it treat those messages as a contract: an
// edit to root.go's "error:" prefix or the wording breaks them by design.
func wantStderrExact(t *testing.T, r result, want string) {
	t.Helper()
	if r.stderr != want {
		t.Errorf("afon stderr =\n  %q\nwant\n  %q", r.stderr, want)
	}
}

func TestCLIUnknownOutputFormat(t *testing.T) {
	r := runAf(t, nil, "inspect", "-o", "yaml")
	wantExit(t, r, "inspect", "-o", "yaml")
	wantStderrExact(t, r, "error: unknown output format \"yaml\" (want text or json)\n")
}

func TestCLIUnknownFlag(t *testing.T) {
	r := runAf(t, nil, "inspect", "--bogus")
	wantExit(t, r, "inspect", "--bogus")
	wantStderrExact(t, r, "error: unknown flag: --bogus\n")
}

// Contains, not exact: cobra's suggestions are deterministic for a given
// command name (fixed Levenshtein threshold), but the list is coupled to
// cobra internals (threshold constants, ordering) — an exact match would
// break on a minor cobra bump for no behavioral reason.
func TestCLIUnknownCommand(t *testing.T) {
	r := runAf(t, nil, "badsub")
	wantExit(t, r, "badsub")
	wantStderrContains(t, r, `unknown command "badsub"`)
}

func TestCLIMissingKubeconfigFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-config")
	r := runAf(t, nil, "inspect", "--kubeconfig", missing)
	wantExit(t, r, "inspect", "--kubeconfig", missing)
	wantStderrContains(t, r, "error: loading kubeconfig:")
}

// TestCLINoKubeconfig isolates the default loading path via an empty
// KUBECONFIG file: the three-ways-to-fix message is the common first-run case.
func TestCLINoKubeconfig(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := runAf(t, map[string]string{"KUBECONFIG": empty}, "inspect")
	wantExit(t, r, "inspect")
	wantStderrContains(t, r, "error: no kubeconfig found", "--kubeconfig")
}

func TestCLIUnknownContext(t *testing.T) {
	path := writeKubeconfig(t, fixtureKubeconfig)
	r := runAf(t, nil, "inspect", "--kubeconfig", path, "--context", "ghost")
	wantExit(t, r, "inspect", "--kubeconfig", path, "--context", "ghost")
	wantStderrExact(t, r, "error: context \"ghost\" does not exist in kubeconfig (available: alpha)\n")
}

// TestCLIClusterDown runs the whole pipeline against a dead local port: the
// classified unreachable error must reach the CLI verbatim.
func TestCLIClusterDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	kc := strings.Replace(fixtureKubeconfig, "https://127.0.0.1:6443", "https://"+dead, 1)
	path := writeKubeconfig(t, kc)

	r := runAf(t, nil, "inspect", "--kubeconfig", path)
	wantExit(t, r, "inspect", "--kubeconfig", path)
	wantStderrContains(t, r,
		`cannot reach cluster "alpha"`,
		"connection refused",
		"is the cluster running?",
	)
}
