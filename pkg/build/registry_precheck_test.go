package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/engine"
	batchv1 "k8s.io/api/batch/v1"
)

// --- reference splitting -------------------------------------------------

func TestSplitRegistryImage(t *testing.T) {
	cases := []struct {
		image   string
		host    string
		repo    string
		wantErr bool
	}{
		{image: "docker.io/example/app", host: "registry-1.docker.io", repo: "example/app"},
		{image: "meowster/afon-demo", host: "registry-1.docker.io", repo: "meowster/afon-demo"},
		{image: "example/app", host: "registry-1.docker.io", repo: "example/app"},
		{image: "app", host: "registry-1.docker.io", repo: "library/app"},
		{image: "ghcr.io/org/img", host: "ghcr.io", repo: "org/img"},
		{image: "localhost:5000/foo", host: "localhost:5000", repo: "foo"},
		{image: "127.0.0.1:1/example/app", host: "127.0.0.1:1", repo: "example/app"},
		{image: "registry.internal:8443/deep/path", host: "registry.internal:8443", repo: "deep/path"},
		{image: "", wantErr: true},
		{image: "docker.io/", wantErr: true},
	}
	for _, tc := range cases {
		host, repo, err := splitRegistryImage(tc.image)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %s/%s", tc.image, host, repo)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.image, err)
			continue
		}
		if host != tc.host || repo != tc.repo {
			t.Errorf("%q = %s/%s, want %s/%s", tc.image, host, repo, tc.host, tc.repo)
		}
	}
}

func TestRegistryProbeScheme(t *testing.T) {
	cases := []struct {
		image  string
		scheme string
	}{
		{"docker.io/example/app", "https"},
		{"ghcr.io/org/img", "https"},
		{"localhost:5000/foo", "http"},
		{"127.0.0.1:1/example/app", "http"},
		{"127.4.5.6/repo", "http"},
	}
	for _, tc := range cases {
		p, err := newRegistryProbe(tc.image)
		if err != nil {
			t.Errorf("%q: %v", tc.image, err)
			continue
		}
		if p.scheme != tc.scheme {
			t.Errorf("%q scheme = %s, want %s", tc.image, p.scheme, tc.scheme)
		}
	}
}

// --- content key ---------------------------------------------------------

func TestContentKeyIsStableAndOrderIndependent(t *testing.T) {
	base := func() *imageBuildSpec {
		return &imageBuildSpec{
			Image: "example/app", SHA: "abc123", Dockerfile: "Dockerfile", Context: ".",
			Args: map[string]string{"B": "2", "A": "1"},
		}
	}
	a, b := base(), base()
	if a.contentKey() != b.contentKey() {
		t.Fatalf("same content must give the same key: %s vs %s", a.contentKey(), b.contentKey())
	}
	if len(a.contentKey()) != 64 || !isHex(a.contentKey()) {
		t.Errorf("content key = %q, want 64 hex chars", a.contentKey())
	}

	// Arg insertion order must not matter (map iteration is random).
	c := base()
	c.Args = map[string]string{"A": "1", "B": "2"}
	if a.contentKey() != c.contentKey() {
		t.Errorf("arg order changed the key")
	}

	// Every input that changes what is built changes the key.
	for name, mutate := range map[string]func(*imageBuildSpec){
		"sha":        func(s *imageBuildSpec) { s.SHA = "def" },
		"image":      func(s *imageBuildSpec) { s.Image = "other/app" },
		"dockerfile": func(s *imageBuildSpec) { s.Dockerfile = "Other" },
		"context":    func(s *imageBuildSpec) { s.Context = "sub" },
		"target":     func(s *imageBuildSpec) { s.Target = "prod" },
		"platform":   func(s *imageBuildSpec) { s.Platform = "linux/arm64" },
		"arg":        func(s *imageBuildSpec) { s.Args["A"] = "9" },
	} {
		s := base()
		mutate(s)
		if s.contentKey() == a.contentKey() {
			t.Errorf("changing %s must change the content key", name)
		}
	}

	// Repo is deliberately excluded (sha pins content) — same rule as
	// the disk cache.
	repo := base()
	repo.Repo = "https://other.example/x.git"
	if repo.contentKey() != a.contentKey() {
		t.Errorf("repo must not change the content key (sha pins content)")
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// --- challenge parsing ---------------------------------------------------

func TestParseChallengeParams(t *testing.T) {
	got := parseChallengeParams(`realm="https://auth.example/token",service="registry.test",scope="repository:example/app:pull"`)
	if got["realm"] != "https://auth.example/token" || got["service"] != "registry.test" ||
		got["scope"] != "repository:example/app:pull" {
		t.Errorf("params = %v", got)
	}
	// A multi-scope value contains spaces but no comma; a comma inside
	// quotes must not split the pair.
	got = parseChallengeParams(`realm="https://a.example/t",scope="repository:x/y:pull repository:z/w:push"`)
	if got["scope"] != "repository:x/y:pull repository:z/w:push" {
		t.Errorf("scope = %q", got["scope"])
	}
}

// --- probe against a fixture registry ------------------------------------

// probeRegistry is a mutable fixture: the handler reads its fields at
// request time, so a test can start the server (learning its host)
// before computing the content key the labels must carry.
type probeRegistry struct {
	labels       map[string]string
	manifestDgst string // Docker-Content-Digest header value; "" omits it
	missingTag   bool
	serveIndex   bool
	basicAuth    bool
	bearerAuth   bool
	tokenQuery   url.Values // last token-endpoint query, for asserts
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newProbeFixture serves a registry on loopback plain HTTP — the
// scheme rule in newRegistryProbe makes the probe speak http to it.
func newProbeFixture(t *testing.T, f *probeRegistry) *httptest.Server {
	t.Helper()
	var authURL string
	if f.bearerAuth {
		auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.tokenQuery = r.URL.Query()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"probe-token"}`)
		}))
		t.Cleanup(auth.Close)
		authURL = auth.URL
	}

	// Bodies are built per request: tests may set labels after the
	// server exists (they learn the host from srv.URL first).
	manifestFor := func() []byte {
		configBlob := mustJSON(t, map[string]any{"config": map[string]any{"Labels": f.labels}})
		configDigest := "sha256:" + shaHex(configBlob)
		if f.serveIndex {
			return mustJSON(t, map[string]any{
				"schemaVersion": 2,
				"mediaType":     "application/vnd.oci.image.index.v1+json",
				"manifests": []any{map[string]any{
					"mediaType": "application/vnd.oci.image.manifest.v1+json",
					"digest":    "sha256:" + strings.Repeat("1", 64),
				}},
			})
		}
		return mustJSON(t, map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config": map[string]any{
				"mediaType": "application/vnd.oci.image.config.v1+json",
				"digest":    configDigest,
				"size":      len(configBlob),
			},
			"layers": []any{},
		})
	}
	configBlobFor := func() []byte {
		return mustJSON(t, map[string]any{"config": map[string]any{"Labels": f.labels}})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case f.missingTag && strings.Contains(r.URL.Path, "/manifests/"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "/manifests/"):
			if f.basicAuth {
				w.Header().Set("WWW-Authenticate", `Basic realm="probe"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if f.bearerAuth {
				if auth := r.Header.Get("Authorization"); auth == "" {
					w.Header().Set("WWW-Authenticate",
						fmt.Sprintf(`Bearer realm=%q,service=%q,scope=%q`,
							authURL, "registry.test", "repository:example/app:pull"))
					w.WriteHeader(http.StatusUnauthorized)
					return
				} else if auth != "Bearer probe-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}
			if f.manifestDgst != "" {
				w.Header().Set("Docker-Content-Digest", f.manifestDgst)
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Write(manifestFor())
		case strings.Contains(r.URL.Path, "/blobs/"):
			w.Write(configBlobFor())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// probeVia builds a probe against the fixture's loopback host.
func probeVia(t *testing.T, srv *httptest.Server, tag, want string) (string, bool) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	p, err := newRegistryProbe(host + "/example/app")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.probe(ctx, tag, want)
}

func TestRegistryProbeMatchingLabelSkips(t *testing.T) {
	want := strings.Repeat("ab", 32)
	headerDigest := "sha256:" + strings.Repeat("c", 64)
	f := &probeRegistry{
		labels:       map[string]string{configLabelKey: want},
		manifestDgst: headerDigest,
	}
	srv := newProbeFixture(t, f)
	digest, ok := probeVia(t, srv, "testsha", want)
	if !ok {
		t.Fatal("matching label must skip the build")
	}
	if digest != headerDigest {
		t.Errorf("digest = %s, want header value %s", digest, headerDigest)
	}
}

func TestRegistryProbeDigestFromBodyWhenHeaderAbsent(t *testing.T) {
	want := strings.Repeat("cd", 32)
	f := &probeRegistry{labels: map[string]string{configLabelKey: want}}
	srv := newProbeFixture(t, f)
	digest, ok := probeVia(t, srv, "testsha", want)
	if !ok {
		t.Fatal("want skip")
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Errorf("digest = %q, want sha256:<64 hex> computed from the body", digest)
	}
}

func TestRegistryProbeDoubtBuilds(t *testing.T) {
	cases := []struct {
		name string
		f    probeRegistry
		want string // content key the probe is asked to prove
	}{
		{name: "missing tag", f: probeRegistry{missingTag: true}, want: strings.Repeat("aa", 32)},
		{name: "label mismatch", f: probeRegistry{labels: map[string]string{configLabelKey: strings.Repeat("00", 32)}}, want: strings.Repeat("aa", 32)},
		{name: "label absent", f: probeRegistry{labels: map[string]string{"other": "x"}}, want: strings.Repeat("aa", 32)},
		{name: "no labels at all", f: probeRegistry{}, want: strings.Repeat("aa", 32)},
		{name: "index manifest", f: probeRegistry{serveIndex: true, labels: map[string]string{configLabelKey: strings.Repeat("aa", 32)}}, want: strings.Repeat("aa", 32)},
		{name: "basic auth wall", f: probeRegistry{basicAuth: true, labels: map[string]string{configLabelKey: strings.Repeat("aa", 32)}}, want: strings.Repeat("aa", 32)},
		{name: "empty want never matches", f: probeRegistry{}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			srv := newProbeFixture(t, &f)
			if digest, ok := probeVia(t, srv, "testsha", tc.want); ok {
				t.Errorf("doubt must build, got skip with digest %s", digest)
			}
		})
	}
}

func TestRegistryProbeBearerDanceSkips(t *testing.T) {
	want := strings.Repeat("ef", 32)
	headerDigest := "sha256:" + strings.Repeat("e", 64)
	f := &probeRegistry{
		labels:       map[string]string{configLabelKey: want},
		manifestDgst: headerDigest,
		bearerAuth:   true,
	}
	srv := newProbeFixture(t, f)
	digest, ok := probeVia(t, srv, "testsha", want)
	if !ok {
		t.Fatal("public Bearer dance must complete and skip the build")
	}
	if digest != headerDigest {
		t.Errorf("digest = %s, want %s", digest, headerDigest)
	}
	if f.tokenQuery.Get("scope") != "repository:example/app:pull" {
		t.Errorf("token scope = %q, want the challenge's scope passed through", f.tokenQuery.Get("scope"))
	}
	if f.tokenQuery.Get("service") != "registry.test" {
		t.Errorf("token service = %q", f.tokenQuery.Get("service"))
	}
}

func TestRegistryProbeNetworkErrorBuilds(t *testing.T) {
	f := &probeRegistry{labels: map[string]string{configLabelKey: strings.Repeat("aa", 32)}}
	srv := newProbeFixture(t, f)
	srv.Close() // nothing listens anymore
	if digest, ok := probeVia(t, srv, "testsha", strings.Repeat("aa", 32)); ok {
		t.Errorf("transport failure must build, got skip with digest %s", digest)
	}
}

// --- RunCluster wiring ---------------------------------------------------

// TestRunClusterRegistryMatchSkipsJob proves the full path: a cache
// miss whose registry already holds the exact build returns the
// registry's digest without ever creating a Job; a mismatched label
// falls through to the Job as before.
func TestRunClusterRegistryMatchSkipsJob(t *testing.T) {
	f := &probeRegistry{manifestDgst: "sha256:" + strings.Repeat("f", 64)}
	srv := newProbeFixture(t, f)

	cfg := validCfg()
	cfg["image"] = strings.TrimPrefix(srv.URL, "http://") + "/example/app"
	spec, err := imageBuildConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.labels = map[string]string{configLabelKey: spec.contentKey()} // set after the host is known

	var job *batchv1.Job
	client := captureJobClient(t, testDigest, &job)
	outs, err := (ImageBuild{}).RunCluster(context.Background(), cfg, engine.Inputs{}, fastRunner(client))
	if err != nil {
		t.Fatalf("RunCluster: %v", err)
	}
	if job != nil {
		t.Fatal("registry match must skip the build Job entirely")
	}
	if v := outs["digest"]; !v.Known || v.Data != f.manifestDgst {
		t.Errorf("digest output = %+v, want registry digest %s", v, f.manifestDgst)
	}
	if v := outs["ref"]; !v.Known || v.Data != cfg["image"].(string)+"@"+f.manifestDgst {
		t.Errorf("ref output = %+v", v)
	}

	// Mismatch → the Job runs (the normal path).
	f.labels = map[string]string{configLabelKey: strings.Repeat("00", 32)}
	job = nil
	outs, err = (ImageBuild{}).RunCluster(context.Background(), cfg, engine.Inputs{}, fastRunner(client))
	if err != nil {
		t.Fatalf("RunCluster (mismatch): %v", err)
	}
	if job == nil {
		t.Fatal("label mismatch must fall through to the build Job")
	}
	if v := outs["digest"]; !v.Known || v.Data != testDigest {
		t.Errorf("digest output = %+v, want the Job's digest %s", v, testDigest)
	}
}
