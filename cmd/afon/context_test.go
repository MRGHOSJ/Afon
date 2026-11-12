package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveContext(t *testing.T) {
	cases := []struct {
		name     string
		flagCtx  string
		flowCtx  string
		want     string
		wantErr  string
		wantWays bool // error must name both ways out
	}{
		{name: "both agree", flagCtx: "k3d-dev", flowCtx: "k3d-dev", want: "k3d-dev"},
		{name: "both disagree", flagCtx: "alpha", flowCtx: "beta", wantErr: "disagrees", wantWays: true},
		{name: "flag only", flagCtx: "alpha", want: "alpha"},
		{name: "flow only", flowCtx: "beta", want: "beta"},
		{name: "neither", wantErr: "spec.target.context", wantWays: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveContext(tc.flagCtx, tc.flowCtx)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if tc.wantWays {
					for _, way := range []string{"--context", "target.context"} {
						if !strings.Contains(err.Error(), way) {
							t.Errorf("err = %v, must name %q as a way out", err, way)
						}
					}
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveContext = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// twoContextKubeconfig has two contexts on different servers so the
// loader test can tell which one won.
const twoContextKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: ca
  cluster:
    server: https://alpha.example:6443
- name: cb
  cluster:
    server: https://beta.example:6443
contexts:
- name: alpha
  context: {cluster: ca, user: u1}
- name: beta
  context: {cluster: cb, user: u1}
current-context: alpha
users:
- name: u1
  user: {}
`

func TestClientConfigFor_TwoContexts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(twoContextKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	old := kubeconfig
	kubeconfig = path
	defer func() { kubeconfig = old }()

	cases := []struct {
		ctx      string
		wantHost string
	}{
		{"alpha", "https://alpha.example:6443"},
		{"beta", "https://beta.example:6443"},
		{"", "https://alpha.example:6443"}, // empty → current context
	}
	for _, tc := range cases {
		cfg, err := clientConfigFor(tc.ctx).ClientConfig()
		if err != nil {
			t.Fatalf("ClientConfig(%q): %v", tc.ctx, err)
		}
		if cfg.Host != tc.wantHost {
			t.Errorf("context %q → host %q, want %q", tc.ctx, cfg.Host, tc.wantHost)
		}
	}
}
