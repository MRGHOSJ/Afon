package build

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MRGHOSJ/afon/pkg/engine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Pinned images (Stage 7 locks exact tags; see the 7f VERIFY report
// for the flags/paths proven against them).
const (
	// buildkitImage is the rootless BuildKit daemonless runner —
	// verified v0.35.0: buildctl-daemonless.sh, busybox sh/sed/grep,
	// metadata file key containerimage.digest (pretty-printed JSON).
	buildkitImage = "moby/buildkit:v0.35.0-rootless"
	// gitInitImage is the init-container checkout tool — verified
	// v2.54.0: git + sh present, image USER is root (the pod pins
	// runAsUser 1000 explicitly).
	gitInitImage = "alpine/git:v2.54.0"
)

// buildPodSeccomp is the evidence-driven Pod Security point (k3d-dev,
// k3s v1.35.5+k3s1, live evidence rounds 2026-11-14): RuntimeDefault
// blocks rootlesskit's own re-exec
// ("rootlesskit: fork/exec /proc/self/exe: operation not permitted"),
// so the pod must run seccomp Unconfined — which the PSA restricted
// level forbids, so the build namespace (default afon-build) must
// carry pod-security.kubernetes.io/enforce: privileged labels. Afon
// never edits namespace labels; the required labels are documented in
// design.md and echoed by the admission-refusal hint on the Job.
const buildPodSeccomp = corev1.SeccompProfileTypeUnconfined

const (
	workspaceVolume   = "workspace"
	dockerCfgVolume   = "docker-config"
	gitAuthVolume     = "git-auth"
	dockerCfgMount    = "/auth"
	gitAuthMount      = "/afon/git-auth"
	workspaceMount    = "/workspace"
	terminationLog    = "/dev/termination-log"
	metadataFilePath  = "/tmp/afon-metadata.json"
	buildUser         = int64(1000)
	defaultTimeout    = 20 * time.Minute
	defaultNamespace  = "afon-build"
	dockerConfigValue = dockerCfgMount // DOCKER_CONFIG env: buildctl reads $DOCKER_CONFIG/config.json
)

// ImageBuild runs a rootless BuildKit build inside a throwaway Job
// and publishes the pushed image digest. Config is {image, repo, sha,
// dockerfile?, context?, target?, args?, platform?, namespace?,
// registrySecret, gitSecret?, resources?, timeout?} — image carries no
// tag (the tag is always the resolved git sha; :latest is impossible
// by design) and secrets are referenced by name only, living in the
// BUILD namespace (default afon-build), never the app namespace.
type ImageBuild struct{}

var (
	_ engine.ActionType   = ImageBuild{}
	_ engine.InputCacher  = ImageBuild{}
	_ engine.ClusterAware = ImageBuild{}
)

// Type implements engine.NodeType.
func (ImageBuild) Type() string { return "image-build" }

// TypeVersion implements engine.NodeType. Bump when the job shape or
// cache inputs change.
func (ImageBuild) TypeVersion() string { return "1" }

// Kind implements engine.NodeType.
func (ImageBuild) Kind() engine.Kind { return engine.KindAction }

// DeclaredOutputs implements engine.NodeType: the pushed content
// digest and the digest-pinned reference the deploy consumes.
func (ImageBuild) DeclaredOutputs() []string { return []string{"digest", "ref"} }

// Validate implements engine.NodeType — structural shape only, with
// Reference/Template markers accepted wherever values may be
// substituted later. Literal values get the full checks now; resolved
// values get the same checks again in imageBuildConfig at run time.
func (ImageBuild) Validate(cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a mapping")
	}

	// Required: image, repo, sha, registrySecret.
	for _, key := range []string{"image", "repo", "sha", "registrySecret"} {
		v, present := m[key]
		if !present {
			return fmt.Errorf("missing required field %q", key)
		}
		if s, isStr := v.(string); isStr {
			if s == "" {
				return fmt.Errorf("%s must not be empty", key)
			}
			if key == "image" {
				if err := checkImageTagless(s); err != nil {
					return err
				}
			}
		} else if !isRefLike(v) {
			return fmt.Errorf("%s must be a string, got %T", key, v)
		}
	}

	// Optional strings: namespace, gitSecret, target, platform.
	for _, key := range []string{"namespace", "gitSecret", "target", "platform"} {
		if v, present := m[key]; present {
			s, isStr := v.(string)
			if !isStr {
				if isRefLike(v) {
					continue
				}
				return fmt.Errorf("%s must be a string, got %T", key, v)
			}
			if key == "namespace" && s == "" {
				return fmt.Errorf("namespace must not be empty")
			}
		}
	}

	// Optional paths: dockerfile (default Dockerfile), context (default .).
	for _, key := range []string{"dockerfile", "context"} {
		if v, present := m[key]; present {
			if s, isStr := v.(string); isStr {
				if err := checkRelPath(key, s); err != nil {
					return err
				}
			} else if !isRefLike(v) {
				return fmt.Errorf("%s must be a string, got %T", key, v)
			}
		}
	}

	if v, present := m["args"]; present {
		args, isMap := v.(map[string]any)
		if !isMap {
			return fmt.Errorf("args must be a mapping, got %T", v)
		}
		for k, val := range args {
			if k == "" || strings.ContainsAny(k, "=\t\r\n ") {
				return fmt.Errorf("args key %q is invalid (no '=' or whitespace)", k)
			}
			if _, isStr := val.(string); !isStr && !isRefLike(val) {
				return fmt.Errorf("args[%q] must be a string, got %T", k, val)
			}
		}
	}

	if v, present := m["timeout"]; present {
		s, isStr := v.(string)
		if !isStr {
			if !isRefLike(v) {
				return fmt.Errorf("timeout must be a duration string, got %T", v)
			}
		} else if d, err := time.ParseDuration(s); err != nil || d <= 0 {
			return fmt.Errorf("timeout must be a positive Go duration (e.g. 20m), got %q", s)
		}
	}

	if v, present := m["resources"]; present {
		if err := validateResources(v); err != nil {
			return err
		}
	}

	known := map[string]bool{
		"image": true, "repo": true, "sha": true, "dockerfile": true, "context": true,
		"target": true, "args": true, "platform": true, "namespace": true,
		"registrySecret": true, "gitSecret": true, "resources": true, "timeout": true,
	}
	for k := range m {
		if !known[k] {
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

// CacheInputs implements engine.InputCacher: exactly the fields the
// brief defines — sha, image, dockerfile, context, target, platform,
// sorted args. Deliberately excluded: repo (location, not content —
// sha pins content), secret names, namespace, resources, timeout
// (they change how/where the build runs, never what it produces), and
// mutable base-image tags inside the Dockerfile (documented
// limitation: changing a floating base tag alone does not rebuild).
func (ImageBuild) CacheInputs(cfg any, in engine.Inputs) (string, error) {
	s, err := imageBuildConfig(cfg)
	if err != nil {
		return "", err
	}
	return s.cacheKey(), nil
}

// Run implements engine.ActionType without a cluster: plan, validate,
// and render never carry one, so the no-cluster entry point is a clear
// refusal (ClusterAware is consulted only on apply; cache hits skip
// both).
func (ImageBuild) Run(ctx context.Context, cfg any, in engine.Inputs) (engine.Outputs, error) {
	return nil, fmt.Errorf("image-build: requires the k8s driver (apply only); builds run as a throwaway Kubernetes Job")
}

// RunCluster implements engine.ClusterAware: build the throwaway Job,
// run it, and turn the termination message into digest + ref.
func (ImageBuild) RunCluster(ctx context.Context, cfg any, in engine.Inputs, cluster any) (engine.Outputs, error) {
	runner, ok := cluster.(*Runner)
	if !ok {
		return nil, fmt.Errorf("image-build: unexpected cluster runner type %T (internal: ExecutorOptions.Cluster must hold *build.Runner)", cluster)
	}
	if runner == nil {
		return nil, fmt.Errorf("image-build: cluster runner is nil (internal: ExecutorOptions.Cluster must hold a constructed *build.Runner)")
	}
	s, err := imageBuildConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("image-build: %w", err)
	}
	runID, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("image-build: generating run id: %w", err)
	}
	job := newJob(JobParams{
		Name:      jobNameFor(runID),
		RunID:     runID,
		Namespace: s.Namespace,
		Timeout:   s.Timeout,
		Init:      []corev1.Container{gitInitContainer(s)},
		Build:     buildContainer(s),
		Volumes:   buildVolumes(s),
	})
	podSec := buildPodSecurityContext()
	job.Spec.Template.Spec.SecurityContext = podSec

	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	msg, err := runner.RunJob(ctx, s.Namespace, job, BuildContainerName)
	if err != nil {
		return nil, fmt.Errorf("image-build: %w", err)
	}
	digest, err := parseDigest(msg)
	if err != nil {
		return nil, err
	}
	return engine.Outputs{
		"digest": engine.KnownValue(digest),
		"ref":    engine.KnownValue(s.Image + "@" + digest),
	}, nil
}

// imageBuildSpec is the concrete, defaulted config both CacheInputs
// and RunCluster operate on (markers are resolved before they run).
type imageBuildSpec struct {
	Image, Repo, SHA    string
	Dockerfile, Context string
	Target, Platform    string
	Namespace           string
	RegistrySecret      string
	GitSecret           string
	Args                map[string]string
	Timeout             time.Duration
	Resources           corev1.ResourceRequirements
}

func imageBuildConfig(cfg any) (*imageBuildSpec, error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config must be a mapping")
	}
	s := &imageBuildSpec{
		Dockerfile: "Dockerfile",
		Context:    ".",
		Namespace:  defaultNamespace,
		Timeout:    defaultTimeout,
		Resources:  defaultResources(),
	}
	for _, key := range []string{"image", "repo", "sha", "registrySecret"} {
		v, present := m[key]
		if !present {
			return nil, fmt.Errorf("missing required field %q", key)
		}
		str, ok := v.(string)
		if !ok || str == "" {
			return nil, fmt.Errorf("%s must resolve to a non-empty string, got %T", key, v)
		}
		switch key {
		case "image":
			if err := checkImageTagless(str); err != nil {
				return nil, err
			}
			s.Image = str
		case "repo":
			s.Repo = str
		case "sha":
			s.SHA = str
		case "registrySecret":
			s.RegistrySecret = str
		}
	}
	optStr := func(key string, dst *string, requiredEmptyOK bool) error {
		v, present := m[key]
		if !present {
			return nil
		}
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s must resolve to a string, got %T", key, v)
		}
		if str == "" && !requiredEmptyOK {
			return fmt.Errorf("%s must not be empty", key)
		}
		*dst = str
		return nil
	}
	if err := optStr("dockerfile", &s.Dockerfile, false); err != nil {
		return nil, err
	}
	if err := optStr("context", &s.Context, false); err != nil {
		return nil, err
	}
	if err := optStr("target", &s.Target, true); err != nil {
		return nil, err
	}
	if err := optStr("platform", &s.Platform, true); err != nil {
		return nil, err
	}
	if err := optStr("namespace", &s.Namespace, false); err != nil {
		return nil, err
	}
	if err := optStr("gitSecret", &s.GitSecret, true); err != nil {
		return nil, err
	}
	if err := checkRelPath("dockerfile", s.Dockerfile); err != nil {
		return nil, err
	}
	if err := checkRelPath("context", s.Context); err != nil {
		return nil, err
	}
	if v, present := m["args"]; present {
		raw, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("args must be a mapping, got %T", v)
		}
		s.Args = make(map[string]string, len(raw))
		for k, val := range raw {
			str, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("args[%q] must resolve to a string, got %T", k, val)
			}
			s.Args[k] = str
		}
	}
	if v, present := m["timeout"]; present {
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("timeout must resolve to a string, got %T", v)
		}
		d, err := time.ParseDuration(str)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("timeout must be a positive duration, got %q", str)
		}
		s.Timeout = d
	}
	if v, present := m["resources"]; present {
		res, err := parseResources(v)
		if err != nil {
			return nil, err
		}
		s.Resources = res
	}
	return s, nil
}

// cacheKey is the canonical CacheInputs string: fixed fields in a
// fixed order, then sorted args.
func (s *imageBuildSpec) cacheKey() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sha=%s\nimage=%s\ndockerfile=%s\ncontext=%s\ntarget=%s\nplatform=%s",
		s.SHA, s.Image, s.Dockerfile, s.Context, s.Target, s.Platform)
	keys := make([]string, 0, len(s.Args))
	for k := range s.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "\narg:%s=%s", k, s.Args[k])
	}
	return b.String()
}

// checkImageTagless rejects an explicit tag: image-build always tags
// <repo>:<git-sha> and deploys by @digest, so :latest (or any tag) in
// config would be silently overwritten.
func checkImageTagless(v string) error {
	last := v[strings.LastIndex(v, "/")+1:]
	if strings.Contains(last, ":") {
		return fmt.Errorf("image must not include a tag (found %q); image-build always tags as the resolved git sha and cannot produce :latest by design", v)
	}
	return nil
}

// checkRelPath keeps dockerfile/context inside the checkout: relative,
// forward slashes only, no ".." escapes, no absolute or drive paths.
func checkRelPath(key, v string) error {
	if v == "" {
		return fmt.Errorf("%s must not be empty", key)
	}
	if strings.Contains(v, `\`) {
		return fmt.Errorf("%s must use forward slashes, got %q", key, v)
	}
	if strings.HasPrefix(v, "/") || (len(v) >= 2 && v[1] == ':') {
		return fmt.Errorf("%s must be relative to the checkout, got %q", key, v)
	}
	for _, elem := range strings.Split(v, "/") {
		if elem == ".." {
			return fmt.Errorf("%s must not escape the checkout (..), got %q", key, v)
		}
	}
	return nil
}

func defaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
}

func validateResources(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("resources must be a mapping, got %T", v)
	}
	for section, val := range m {
		if section != "requests" && section != "limits" {
			return fmt.Errorf("resources has unknown section %q (want requests or limits)", section)
		}
		inner, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("resources.%s must be a mapping, got %T", section, val)
		}
		for k, q := range inner {
			if k != "cpu" && k != "memory" {
				return fmt.Errorf("resources.%s has unknown key %q (want cpu or memory)", section, k)
			}
			s, isStr := q.(string)
			if !isStr || s == "" {
				return fmt.Errorf("resources.%s.%s must be a quantity string, got %T", section, k, q)
			}
			if _, err := resource.ParseQuantity(s); err != nil {
				return fmt.Errorf("resources.%s.%s: %v", section, k, err)
			}
		}
	}
	return nil
}

// parseResources overlays the user's requested sections onto the
// defaults (partial overrides are allowed: limits-only, cpu-only).
func parseResources(v any) (corev1.ResourceRequirements, error) {
	out := defaultResources()
	m, ok := v.(map[string]any)
	if !ok {
		return out, fmt.Errorf("resources must be a mapping, got %T", v)
	}
	for section, val := range m {
		inner, ok := val.(map[string]any)
		if !ok {
			return out, fmt.Errorf("resources.%s must be a mapping, got %T", section, val)
		}
		list := corev1.ResourceList{}
		for k, q := range inner {
			s, isStr := q.(string)
			if !isStr {
				return out, fmt.Errorf("resources.%s.%s must be a string, got %T", section, k, q)
			}
			quantity, err := resource.ParseQuantity(s)
			if err != nil {
				return out, fmt.Errorf("resources.%s.%s: %v", section, k, err)
			}
			list[corev1.ResourceName(k)] = quantity
		}
		switch section {
		case "requests":
			out.Requests = list
		case "limits":
			out.Limits = list
		default:
			return out, fmt.Errorf("resources has unknown section %q (want requests or limits)", section)
		}
	}
	return out, nil
}

// shQuote makes any value safe inside a single-quoted sh word: values
// flow from flow.yaml (refs may contain shell metacharacters), so
// every interpolated value is quoted this way — no injection into the
// pod's shell scripts.
func shQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// gitInitContainer fetches exactly the resolved SHA into the shared
// checkout. All dynamic values are shQuote-wrapped. The credential
// helper only exists when the optional git Secret mounted its
// `credential` key (v1 = HTTPS PAT; SSH arrives with afon secret
// set-git).
func gitInitContainer(s *imageBuildSpec) corev1.Container {
	script := fmt.Sprintf(`set -eu
cd %s
git init -q .
git remote add origin %s
if [ -f %s/credential ]; then
  git config credential.helper '!f() { echo username=git; echo password="$(cat %s/credential)"; }; f'
fi
git fetch -q --depth 1 origin %s 2>/dev/null || git fetch -q origin
git checkout -q -f %s
`, shQuote(workspaceMount), shQuote(s.Repo), shQuote(gitAuthMount), shQuote(gitAuthMount), shQuote(s.SHA), shQuote(s.SHA))

	c := corev1.Container{
		Name:            GitInitContainerName,
		Image:           gitInitImage,
		Command:         []string{"sh", "-c"},
		Args:            []string{script},
		ImagePullPolicy: corev1.PullIfNotPresent,
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolume, MountPath: workspaceMount},
		},
		Env: []corev1.EnvVar{{Name: "GIT_TERMINAL_PROMPT", Value: "0"}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    ptr(buildUser),
			RunAsGroup:   ptr(buildUser),
			RunAsNonRoot: ptr(true),
			// git writes the checkout as 1000:1000; fsGroup keeps the
			// shared workspace group-consistent for BuildKit.
			AllowPrivilegeEscalation: ptr(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if s.GitSecret != "" {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name: gitAuthVolume, MountPath: gitAuthMount, ReadOnly: true,
		})
	}
	return c
}

// buildContainer runs the daemonless BuildKit build and writes ONLY
// the pushed digest line to the termination log (the metadata JSON
// can exceed the 4096-byte message cap; verified key is
// "containerimage.digest" with pretty-printed spacing). On buildctl
// failure `set -e` stops before sed, leaving the log empty so
// terminationMessagePolicy=FallbackToLogsOnError surfaces the last
// log chunk as the container message.
func buildContainer(s *imageBuildSpec) corev1.Container {
	ctxDir := path.Join(workspaceMount, s.Context)
	var b strings.Builder
	b.WriteString("set -eu\nbuildctl-daemonless.sh build --frontend dockerfile.v0")
	fmt.Fprintf(&b, " --local %s --local %s",
		shQuote("context="+ctxDir), shQuote("dockerfile="+ctxDir))
	fmt.Fprintf(&b, " --opt %s", shQuote("filename="+s.Dockerfile))
	if s.Target != "" {
		fmt.Fprintf(&b, " --opt %s", shQuote("target="+s.Target))
	}
	if s.Platform != "" {
		fmt.Fprintf(&b, " --opt %s", shQuote("platform="+s.Platform))
	}
	keys := make([]string, 0, len(s.Args))
	for k := range s.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " --opt %s", shQuote("build-arg:"+k+"="+s.Args[k]))
	}
	fmt.Fprintf(&b, " --output %s --metadata-file %s\n",
		shQuote("type=image,name="+s.Image+":"+s.SHA+",push=true"), metadataFilePath)
	fmt.Fprintf(&b, "sed -n %s %s > %s",
		shQuote(`s/.*"containerimage\.digest": *"\([^"]*\)".*/\1/p`), metadataFilePath, terminationLog)

	return corev1.Container{
		Name:            BuildContainerName,
		Image:           buildkitImage,
		Command:         []string{"sh", "-c"},
		Args:            []string{b.String()},
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: "DOCKER_CONFIG", Value: dockerConfigValue},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolume, MountPath: workspaceMount},
			{Name: dockerCfgVolume, MountPath: dockerCfgMount, ReadOnly: true},
		},
		Resources: s.Resources,
		// Digest on success (written above); on failure the empty log
		// falls back to the tail of buildctl's output for the error.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot: ptr(true),
			// Evidence (rounds b1→b3 on k3d-dev): newuidmap ships a
			// security.capability xattr; with drop:ALL and an EMPTY
			// bounding set its exec is denied EPERM even at
			// NoNewPrivs=0, which wedges rootlesskit's uid/gid map
			// setup. Keeping SETUID+SETGID in the bounding set (and
			// allowing their elevation) makes the full rootless build
			// succeed. Requires the privileged-level build namespace
			// (PSA restricted/baseline forbid added caps and AE=true).
			AllowPrivilegeEscalation: ptr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"SETUID", "SETGID"},
			},
		},
	}
}

// buildVolumes: the shared checkout (emptyDir), the registry
// credential (explicit items projection — key .dockerconfigjson, the
// key kubectl create secret docker-registry writes, presented as
// config.json under DOCKER_CONFIG), and optionally the git credential
// (key credential, init container only). A wrong-shaped Secret fails
// at mount time with a FailedMount event — Afon never reads Secrets.
func buildVolumes(s *imageBuildSpec) []corev1.Volume {
	mode := int32(0644)
	vols := []corev1.Volume{
		{Name: workspaceVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{
			Name: dockerCfgVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  s.RegistrySecret,
					Items:       []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
					DefaultMode: &mode,
				},
			},
		},
	}
	if s.GitSecret != "" {
		vols = append(vols, corev1.Volume{
			Name: gitAuthVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  s.GitSecret,
					Items:       []corev1.KeyToPath{{Key: "credential", Path: "credential"}},
					DefaultMode: &mode,
				},
			},
		})
	}
	return vols
}

// buildPodSecurityContext is the pod-level posture: fsGroup 1000 so
// the shared workspace and mounts are group-consistent, and the
// evidence-driven seccomp choice (Unconfined — RuntimeDefault blocks
// rootlesskit; see buildPodSeccomp).
func buildPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		FSGroup: ptr(buildUser),
		SeccompProfile: &corev1.SeccompProfile{
			Type: buildPodSeccomp,
		},
	}
}

// parseDigest validates the termination message as exactly
// sha256:<64 hex> (lowercased after trim).
func parseDigest(msg string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return "", fmt.Errorf("image-build: empty termination message; expected the pushed image digest written by the %s container to %s", BuildContainerName, terminationLog)
	}
	if !digestLineRe.MatchString(m) {
		short := m
		if len(short) > 120 {
			short = short[:120] + "..."
		}
		return "", fmt.Errorf("image-build: unexpected termination message %q (want sha256:<64 hex> of the pushed image)", short)
	}
	return m, nil
}

var digestLineRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ptr[T any](v T) *T { return &v }
