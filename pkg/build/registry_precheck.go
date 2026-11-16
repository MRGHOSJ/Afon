package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// configLabelKey is the image label kaniko stamps with the build's
// content key; the anonymous registry probe reads it back to decide
// whether the tag already holds this exact build.
const configLabelKey = "afon.dev/build-config"

// probeTimeout bounds the whole registry probe. It runs before every
// uncached build, so a hung registry may delay — never block — the
// apply, and any overrun simply falls through to the build.
const probeTimeout = 10 * time.Second

// maxProbeBody caps manifest and config reads (both are small; the
// cap exists so a hostile registry cannot stream indefinitely).
const maxProbeBody = 8 << 20

// manifestAccept lists every media type the probe understands, single
// manifest first: the registry answers with the best it has, and an
// index answer is detected by the body (no config.digest) rather than
// by trusting the header alone.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
}, ", ")

// contentKey hex-encodes sha256 of the cache key: the exact value
// stamped into the image via kaniko's --label afon.dev.build-config,
// so a later probe can prove the tag holds this build. Same inputs as
// the disk cache (sha, image, dockerfile, context, target, platform,
// sorted args), one canonical string, fixed 64-hex length for label
// limits.
func (s *imageBuildSpec) contentKey() string {
	sum := sha256.Sum256([]byte(s.cacheKey()))
	return hex.EncodeToString(sum[:])
}

// preCheckRegistry reports whether the registry already holds
// s.Image:s.SHA built from this exact content — in which case the
// caller skips the build Job entirely. Anonymous by design: afon never
// reads the registry Secret here, so any doubt (missing tag, auth wall,
// index manifest, network trouble, absent or mismatched label) returns
// ok=false and the build runs as before.
func preCheckRegistry(ctx context.Context, s *imageBuildSpec) (digest string, ok bool) {
	p, err := newRegistryProbe(s.Image)
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return p.probe(ctx, s.SHA, s.contentKey())
}

// registryProbe is an anonymous Docker Registry API v2 client for one
// image reference.
type registryProbe struct {
	scheme string // http for local registries, https otherwise
	host   string // registry host[:port]
	repo   string // repository path, no leading slash
}

// newRegistryProbe splits an image reference into registry API parts.
func newRegistryProbe(image string) (*registryProbe, error) {
	host, repo, err := splitRegistryImage(image)
	if err != nil {
		return nil, err
	}
	scheme := "https"
	if isLocalHost(host) {
		scheme = "http" // local registries (k3d, dev loops) speak plain HTTP
	}
	return &registryProbe{scheme: scheme, host: host, repo: repo}, nil
}

// splitRegistryImage applies Docker's rule: the first path component
// is a registry iff it contains "." or ":" or is "localhost"; anything
// else is Docker Hub shorthand (org/repo, bare repo → library/repo).
// The docker.io aliases map to the Hub API host.
func splitRegistryImage(image string) (host, repo string, err error) {
	first, rest, found := strings.Cut(image, "/")
	if !found {
		if first == "" {
			return "", "", fmt.Errorf("image %q has no repository path", image)
		}
		return "registry-1.docker.io", "library/" + first, nil
	}
	if first != "localhost" && !strings.Contains(first, ".") && !strings.Contains(first, ":") {
		// Docker Hub shorthand: org/repo (bare repo → library/repo
		// is handled by the no-slash branch above).
		return "registry-1.docker.io", first + "/" + rest, nil
	}
	if rest == "" {
		return "", "", fmt.Errorf("image %q has no repository path", image)
	}
	return normalizeHubHost(first), rest, nil
}

func normalizeHubHost(host string) string {
	switch host {
	case "docker.io", "index.docker.io", "registry.docker.io":
		return "registry-1.docker.io"
	}
	return host
}

// isLocalHost reports whether the registry host is loopback — the
// plain-HTTP case (k3d and dev registries terminate TLS nowhere).
func isLocalHost(host string) bool {
	h := host
	if strings.HasPrefix(h, "[") { // [::1]:5000
		if i := strings.Index(h, "]"); i >= 0 {
			h = h[1:i]
		}
	} else if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == "localhost" || h == "::1" || h == "127.0.0.1" || strings.HasPrefix(h, "127.")
}

// probe returns the manifest digest when the tag exists and its image
// config carries configLabelKey == want. Every other outcome — missing
// tag, a challenge it cannot satisfy anonymously, an index manifest, a
// label that is absent or different, any transport error — is
// ("", false): doubt builds.
func (p *registryProbe) probe(ctx context.Context, tag, want string) (string, bool) {
	if want == "" {
		return "", false
	}
	client := &http.Client{}
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", p.scheme, p.host, p.repo, tag)
	status, header, body := p.get(ctx, client, manifestURL, manifestAccept)
	if status != http.StatusOK {
		return "", false
	}
	// A single-manifest body has config.digest; an index (multi-arch)
	// does not, and gets built — its per-platform configs are not this
	// probe's business.
	var man struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &man); err != nil || man.Config.Digest == "" {
		return "", false
	}
	digest := header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	blobURL := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", p.scheme, p.host, p.repo, man.Config.Digest)
	status, _, body = p.get(ctx, client, blobURL, "")
	if status != http.StatusOK {
		return "", false
	}
	var img struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &img); err != nil {
		return "", false
	}
	if img.Config.Labels[configLabelKey] != want {
		return "", false
	}
	return digest, true
}

// get performs one anonymous GET, completing a Bearer challenge (the
// public-registry token dance) exactly once. A transport failure
// returns (0, nil, nil); non-200 statuses return as answered so the
// caller can tell 404 from 401 from 500 — all of which build.
func (p *registryProbe) get(ctx context.Context, client *http.Client, url, accept string) (int, http.Header, []byte) {
	token := ""
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, nil, nil
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
		resp.Body.Close()
		if readErr != nil {
			return 0, nil, nil
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			t, ok := anonymousToken(ctx, client, resp.Header.Get("WWW-Authenticate"))
			if !ok {
				return resp.StatusCode, resp.Header, body
			}
			token = t
			continue
		}
		return resp.StatusCode, resp.Header, body
	}
	return 0, nil, nil
}

// anonymousToken answers a Bearer challenge with no credentials — the
// public-pull dance every Hub-style registry speaks. Basic challenges
// (or a token endpoint that refuses) return ok=false: the caller falls
// back to building, where the Secret-mounted credentials do the push.
func anonymousToken(ctx context.Context, client *http.Client, challenge string) (string, bool) {
	const bearer = "Bearer "
	if !strings.HasPrefix(challenge, bearer) {
		return "", false
	}
	params := parseChallengeParams(challenge[len(bearer):])
	realm := params["realm"]
	if realm == "" {
		return "", false
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", false
	}
	q := u.Query()
	if v := params["service"]; v != "" {
		q.Set("service", v)
	}
	if v := params["scope"]; v != "" {
		q.Set("scope", v)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody>>3))
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", false
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", false
	}
	if tok.Token != "" {
		return tok.Token, true
	}
	return tok.AccessToken, tok.AccessToken != ""
}

// parseChallengeParams parses key="value" pairs from a
// WWW-Authenticate challenge value; splitQuotedCommas keeps commas
// inside quotes (scope="repository:a/b:pull ...") intact.
func parseChallengeParams(s string) map[string]string {
	out := map[string]string{}
	for _, part := range splitQuotedCommas(s) {
		k, v, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

func splitQuotedCommas(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}
