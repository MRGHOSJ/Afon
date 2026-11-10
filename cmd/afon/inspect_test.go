package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validKubeconfig = `apiVersion: v1
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

// setKubeconfig points runInspect at a fixture and restores the globals so
// subtests cannot leak state into each other.
func setKubeconfig(t *testing.T, path, context string) {
	t.Helper()
	oldPath, oldContext := kubeconfig, kubeContext
	kubeconfig, kubeContext = path, context
	t.Cleanup(func() { kubeconfig, kubeContext = oldPath, oldContext })
}

func TestRunInspectKubeconfigErrors(t *testing.T) {
	t.Run("empty kubeconfig", func(t *testing.T) {
		setKubeconfig(t, writeKubeconfig(t, ""), "")
		err := runInspect(context.Background())
		if err == nil {
			t.Fatal("runInspect() error = nil")
		}
		if !strings.Contains(err.Error(), "no kubeconfig found") {
			t.Errorf("error = %q, want it to mention no kubeconfig found", err)
		}
		if !strings.Contains(err.Error(), "--kubeconfig") {
			t.Errorf("error = %q, want a --kubeconfig hint", err)
		}
	})

	t.Run("unknown --context lists alternatives", func(t *testing.T) {
		setKubeconfig(t, writeKubeconfig(t, validKubeconfig), "beta")
		err := runInspect(context.Background())
		if err == nil {
			t.Fatal("runInspect() error = nil")
		}
		want := `context "beta" does not exist in kubeconfig (available: alpha)`
		if got := err.Error(); got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})

	t.Run("broken current-context lists alternatives", func(t *testing.T) {
		cfg := strings.Replace(validKubeconfig, "current-context: alpha", "current-context: ghost", 1)
		setKubeconfig(t, writeKubeconfig(t, cfg), "")
		err := runInspect(context.Background())
		if err == nil {
			t.Fatal("runInspect() error = nil")
		}
		want := `context "ghost" does not exist in kubeconfig (available: alpha)`
		if got := err.Error(); got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})

	// The full pipeline against a dead local port: kubeconfig loads, the
	// context validates, every probe fails with connection refused, and the
	// classified unreachable error must come back out.
	t.Run("cluster down", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()

		kc := strings.Replace(validKubeconfig, "https://127.0.0.1:6443", "https://"+addr, 1)
		setKubeconfig(t, writeKubeconfig(t, kc), "")

		err = runInspect(context.Background())
		if err == nil {
			t.Fatal("runInspect() error = nil, want unreachable-cluster error")
		}
		msg := err.Error()
		for _, want := range []string{
			`cannot reach cluster "alpha"`,
			"connection refused",
			"is the cluster running?",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("error = %q, want it to contain %q", msg, want)
			}
		}
	})
}
