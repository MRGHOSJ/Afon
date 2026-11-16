package build

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/engine"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testSHA      = "0123456789abcdef0123456789abcdef0123456789"
	testDigest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testImage    = "127.0.0.1:1/example/app"
	testRepoURL  = "https://github.com/example/app.git"
	testRegistry = "dockerhub-creds"
)

func validCfg() map[string]any {
	return map[string]any{
		"image":          testImage,
		"repo":           testRepoURL,
		"sha":            testSHA,
		"registrySecret": testRegistry,
	}
}

func TestImageBuild_TypeAndOutputs(t *testing.T) {
	n := ImageBuild{}
	if n.Type() != "image-build" || n.TypeVersion() != "2" || n.Kind() != engine.KindAction {
		t.Errorf("meta: %s/%s %s", n.Type(), n.TypeVersion(), n.Kind())
	}
	got := n.DeclaredOutputs()
	if len(got) != 2 || got[0] != "digest" || got[1] != "ref" {
		t.Errorf("outputs = %v", got)
	}
}

func TestImageBuild_ValidateTable(t *testing.T) {
	cases := []struct {
		name string
		cfg  any
		want string
	}{
		{"not a mapping nil", nil, "config must be a mapping"},
		{"not a mapping typed", []string{"x"}, "config must be a mapping"},
		{"missing image", del(validCfg(), "image"), `missing required field "image"`},
		{"missing repo", del(validCfg(), "repo"), `missing required field "repo"`},
		{"missing sha", del(validCfg(), "sha"), `missing required field "sha"`},
		{"missing registry secret", del(validCfg(), "registrySecret"), `missing required field "registrySecret"`},
		{"empty image", set(validCfg(), "image", ""), "image must not be empty"},
		{"tagged image", set(validCfg(), "image", "example/app:v1"), "must not include a tag"},
		{"latest called out", set(validCfg(), "image", "example/app:latest"), "cannot produce :latest by design"},
		{"image wrong type", set(validCfg(), "image", 42), "image must be a string, got int"},
		{"repo wrong type", set(validCfg(), "repo", []any{}), "repo must be a string, got []interface {}"},
		{"absolute dockerfile", set(validCfg(), "dockerfile", "/etc/passwd"), "must be relative"},
		{"dotdot dockerfile", set(validCfg(), "dockerfile", "../Dockerfile"), "must not escape"},
		{"backslash dockerfile", set(validCfg(), "dockerfile", `win\Dockerfile`), "forward slashes"},
		{"empty dockerfile", set(validCfg(), "dockerfile", ""), "dockerfile must not be empty"},
		{"dotdot context", set(validCfg(), "context", "../evil"), "must not escape"},
		{"empty namespace", set(validCfg(), "namespace", ""), "namespace must not be empty"},
		{"args not mapping", set(validCfg(), "args", "nope"), "args must be a mapping"},
		{"args bad key", set(validCfg(), "args", map[string]any{"a=b": "v"}), "args key"},
		{"args empty key", set(validCfg(), "args", map[string]any{"": "v"}), "args key"},
		{"args value type", set(validCfg(), "args", map[string]any{"A": 5}), `args["A"] must be a string, got int`},
		{"timeout not duration", set(validCfg(), "timeout", "20"), "timeout must be a positive Go duration"},
		{"timeout zero", set(validCfg(), "timeout", "0s"), "timeout must be a positive Go duration"},
		{"timeout wrong type", set(validCfg(), "timeout", 5), "timeout must be a duration string"},
		{"resources not mapping", set(validCfg(), "resources", []any{}), "resources must be a mapping"},
		{"resources unknown section", set(validCfg(), "resources", map[string]any{"bursts": map[string]any{}}), "unknown section"},
		{"resources unknown key", set(validCfg(), "resources", map[string]any{"requests": map[string]any{"gpu": "1"}}), "unknown key"},
		{"resources bad quantity", set(validCfg(), "resources", map[string]any{"requests": map[string]any{"cpu": "abc"}}), "resources.requests.cpu"},
		{"resources section type", set(validCfg(), "resources", map[string]any{"requests": "1"}), "resources.requests must be a mapping"},
		{"unknown field", set(validCfg(), "extra", "x"), `unknown field "extra"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (ImageBuild{}).Validate(tc.cfg)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestImageBuild_ValidateAccepts(t *testing.T) {
	markers := map[string]any{
		"image":          engine.Reference{},
		"repo":           engine.Template{},
		"sha":            engine.Reference{},
		"registrySecret": engine.Template{},
		"dockerfile":     engine.Reference{},
		"context":        engine.Reference{},
		"target":         engine.Reference{},
		"platform":       engine.Reference{},
		"namespace":      engine.Reference{},
		"gitSecret":      engine.Reference{},
		"timeout":        engine.Reference{},
		"args":           map[string]any{"V": engine.Reference{}},
		"resources":      map[string]any{"limits": map[string]any{"cpu": "3"}},
	}
	if err := (ImageBuild{}).Validate(markers); err != nil {
		t.Errorf("marker config rejected: %v", err)
	}
	ok := validCfg()
	ok["image"] = "localhost:5000/ns/app" // registry port is not a tag
	ok["gitSecret"] = "git-pat"
	ok["target"] = "prod"
	ok["platform"] = "linux/amd64"
	ok["timeout"] = "1h30m"
	ok["dockerfile"] = "sub/Dockerfile"
	ok["context"] = "app"
	ok["args"] = map[string]any{"MY_ARG": "1", "-flag": "on"}
	if err := (ImageBuild{}).Validate(ok); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestImageBuild_CacheInputsCanonical(t *testing.T) {
	cfg := validCfg()
	cfg["dockerfile"] = "sub/Dockerfile"
	cfg["context"] = "app"
	cfg["target"] = "prod"
	cfg["platform"] = "linux/amd64"
	cfg["args"] = map[string]any{"Z": "9", "A": "1", "M": "2"}
	key, err := (ImageBuild{}).CacheInputs(cfg, engine.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	want := "sha=" + testSHA +
		"\nimage=" + testImage +
		"\ndockerfile=sub/Dockerfile" +
		"\ncontext=app" +
		"\ntarget=prod" +
		"\nplatform=linux/amd64" +
		"\narg:A=1" +
		"\narg:M=2" +
		"\narg:Z=9"
	if key != want {
		t.Errorf("key =\n%q\nwant\n%q", key, want)
	}
}

func TestImageBuild_CacheInputsDefaults(t *testing.T) {
	key, err := (ImageBuild{}).CacheInputs(validCfg(), engine.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(key, "\ndockerfile=Dockerfile\n") ||
		!strings.Contains(key, "\ncontext=.\n") ||
		!strings.Contains(key, "\ntarget=\n") ||
		!strings.Contains(key, "\nplatform=") {
		t.Errorf("defaults missing from key: %q", key)
	}
	if strings.Contains(key, "arg:") {
		t.Errorf("no args expected: %q", key)
	}
}

func TestImageBuild_CacheInputsExcludesNonContentFields(t *testing.T) {
	cfg := validCfg()
	cfg["namespace"] = "other-ns"
	cfg["gitSecret"] = "git-pat"
	cfg["timeout"] = "5m"
	cfg["resources"] = map[string]any{"limits": map[string]any{"cpu": "9"}}
	key, err := (ImageBuild{}).CacheInputs(cfg, engine.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(key, "\n") {
		for _, banned := range []string{"repo=", "namespace=", "registrySecret=", "gitSecret=", "timeout=", "resources="} {
			if strings.HasPrefix(line, banned) {
				t.Errorf("line %q must not appear in the cache key", line)
			}
		}
	}
	if strings.Contains(key, testRepoURL) || strings.Contains(key, testRegistry) {
		t.Errorf("values leaked into key: %q", key)
	}
}

func TestImageBuild_CacheInputsErrors(t *testing.T) {
	if _, err := (ImageBuild{}).CacheInputs("nope", engine.Inputs{}); err == nil {
		t.Error("non-mapping config must fail")
	}
	bad := validCfg()
	bad["image"] = "app:v2"
	if _, err := (ImageBuild{}).CacheInputs(bad, engine.Inputs{}); err == nil ||
		!strings.Contains(err.Error(), "must not include a tag") {
		t.Errorf("resolved tag must fail cache key: %v", err)
	}
}

func TestImageBuild_RunRequiresK8sDriver(t *testing.T) {
	outs, err := (ImageBuild{}).Run(context.Background(), validCfg(), engine.Inputs{})
	if err == nil || !strings.Contains(err.Error(), "requires the k8s driver (apply only)") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "image-build") {
		t.Errorf("err should carry the node name: %v", err)
	}
	if outs != nil {
		t.Errorf("outs = %v, want nil", outs)
	}
}

func TestImageBuild_RunClusterWrongClusterTypes(t *testing.T) {
	ctx := context.Background()
	if _, err := (ImageBuild{}).RunCluster(ctx, validCfg(), engine.Inputs{}, "a-string"); err == nil ||
		!strings.Contains(err.Error(), "unexpected cluster runner type string") {
		t.Errorf("string cluster: %v", err)
	}
	if _, err := (ImageBuild{}).RunCluster(ctx, validCfg(), engine.Inputs{}, nil); err == nil ||
		!strings.Contains(err.Error(), "unexpected cluster runner type <nil>") {
		t.Errorf("nil cluster: %v", err)
	}
	var nilRunner *Runner
	if _, err := (ImageBuild{}).RunCluster(ctx, validCfg(), engine.Inputs{}, nilRunner); err == nil ||
		!strings.Contains(err.Error(), "cluster runner is nil") {
		t.Errorf("typed nil runner: %v", err)
	}
}

func TestImageBuild_ParseDigest(t *testing.T) {
	if got, err := parseDigest("  " + testDigest + "\n"); err != nil || got != testDigest {
		t.Errorf("valid: got %q err %v", got, err)
	}
	upper := "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if got, err := parseDigest(upper); err != nil || got != testDigest {
		t.Errorf("upper: got %q err %v", got, err)
	}
	if _, err := parseDigest(""); err == nil || !strings.Contains(err.Error(), "empty termination message") {
		t.Errorf("empty: %v", err)
	}
	if _, err := parseDigest("   \n"); err == nil || !strings.Contains(err.Error(), "empty termination message") {
		t.Errorf("blank: %v", err)
	}
	if _, err := parseDigest("sha256:short"); err == nil ||
		!strings.Contains(err.Error(), "unexpected termination message") ||
		!strings.Contains(err.Error(), "want sha256:") {
		t.Errorf("short: %v", err)
	}
	long := strings.Repeat("x", 200)
	_, err := parseDigest(long)
	if err == nil || !strings.Contains(err.Error(), strings.Repeat("x", 120)+"...") {
		t.Errorf("long message must truncate: %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("x", 121)) {
		t.Errorf("truncation failed: %v", err)
	}
}

// captureJobClient stores the created Job (for spec assertions) and
// immediately supplies a succeeded pod carrying message — the exact
// path RunCluster exercises through the Runner.
func captureJobClient(t *testing.T, message string, got **batchv1.Job) *fake.Clientset {
	t.Helper()
	client := fake.NewClientset()
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		*got = job.DeepCopy()
		if err := client.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), job, job.Namespace); err != nil {
			return true, nil, err
		}
		p := succeededPod(message)
		p.Namespace = job.Namespace
		p.Labels = map[string]string{RunIDLabel: job.Labels[RunIDLabel]}
		if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
			return true, nil, err
		}
		return true, job, nil
	})
	return client
}

func TestImageBuild_JobSpecDefaults(t *testing.T) {
	var job *batchv1.Job
	client := captureJobClient(t, testDigest, &job)
	outs, err := (ImageBuild{}).RunCluster(context.Background(), validCfg(), engine.Inputs{}, fastRunner(client))
	if err != nil {
		t.Fatalf("RunCluster: %v", err)
	}
	if v := outs["digest"]; !v.Known || v.Data != testDigest {
		t.Errorf("digest output = %+v", v)
	}
	if v := outs["ref"]; !v.Known || v.Data != testImage+"@"+testDigest {
		t.Errorf("ref output = %+v", v)
	}
	if job == nil {
		t.Fatal("job was never created")
	}

	if job.Labels[ManagedByLabel] != ManagedByValue || len(job.Labels[RunIDLabel]) != 16 {
		t.Errorf("labels = %v", job.Labels)
	}
	if *job.Spec.TTLSecondsAfterFinished != 600 || *job.Spec.BackoffLimit != 0 {
		t.Errorf("ttl=%d backoff=%d", *job.Spec.TTLSecondsAfterFinished, *job.Spec.BackoffLimit)
	}
	if *job.Spec.ActiveDeadlineSeconds != int64((20*time.Minute)/time.Second)+60 {
		t.Errorf("activeDeadline = %d, want 1260", *job.Spec.ActiveDeadlineSeconds)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.RestartPolicy)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 1000 {
		t.Errorf("pod fsGroup = %v", pod.SecurityContext)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccomp (kaniko: RuntimeDefault suffices, baseline PSA) = %v", pod.SecurityContext)
	}

	if len(pod.InitContainers) != 1 {
		t.Fatalf("init containers = %d", len(pod.InitContainers))
	}
	init := pod.InitContainers[0]
	if init.Name != GitInitContainerName || init.Image != gitInitImage {
		t.Errorf("init = %s/%s", init.Name, init.Image)
	}
	sc := init.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1000 || sc.RunAsGroup == nil || *sc.RunAsGroup != 1000 ||
		sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("init securityContext = %+v", sc)
	}
	if len(init.Env) != 4 || init.Env[0].Name != "GIT_TERMINAL_PROMPT" || init.Env[0].Value != "0" {
		t.Errorf("init env = %v", init.Env)
	}
	initEnv := map[string]string{}
	for _, e := range init.Env {
		initEnv[e.Name] = e.Value
	}
	if initEnv["GIT_CONFIG_COUNT"] != "1" || initEnv["GIT_CONFIG_KEY_0"] != "safe.directory" || initEnv["GIT_CONFIG_VALUE_0"] != workspaceMount {
		t.Errorf("init env must whitelist the workspace for git (dubious-ownership fix): %v", init.Env)
	}
	initScript := init.Args[0]
	if !strings.Contains(initScript, "git remote add origin "+shQuote(testRepoURL)) {
		t.Errorf("init script missing quoted remote: %s", initScript)
	}
	if strings.Count(initScript, shQuote(testSHA)) != 2 {
		t.Errorf("init script must quote the sha on fetch and checkout: %s", initScript)
	}
	if !strings.Contains(initScript, "git fetch -q --depth 1 origin") ||
		!strings.Contains(initScript, "git fetch -q origin") {
		t.Errorf("init script missing shallow-then-fallback fetch: %s", initScript)
	}
	if !strings.Contains(initScript, `[ -f `+shQuote(gitAuthMount)+`/credential ]`) ||
		!strings.Contains(initScript, "git config credential.helper") {
		t.Errorf("init script credential helper must be mount-guarded: %s", initScript)
	}
	if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].Name != workspaceVolume {
		t.Errorf("init mounts (no git auth expected) = %v", init.VolumeMounts)
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d", len(pod.Containers))
	}
	build := pod.Containers[0]
	if build.Name != BuildContainerName || build.Image != kanikoImage {
		t.Errorf("build = %s/%s", build.Name, build.Image)
	}
	// No shell: the image entrypoint /kaniko/executor receives the
	// flags as argv elements — Command must stay nil (a reintroduced
	// sh -c wrapper is a regression).
	if build.Command != nil {
		t.Errorf("build command = %v, want nil (image entrypoint /kaniko/executor)", build.Command)
	}
	if len(build.Env) != 0 {
		t.Errorf("build env = %v, want none (DOCKER_CONFIG is baked into the kaniko image)", build.Env)
	}
	if build.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("terminationMessagePolicy = %s", build.TerminationMessagePolicy)
	}
	if build.Resources.Requests.Cpu().String() != "1" || build.Resources.Requests.Memory().String() != "2Gi" ||
		build.Resources.Limits.Cpu().String() != "2" || build.Resources.Limits.Memory().String() != "4Gi" {
		t.Errorf("default resources = %v", build.Resources)
	}
	bsc := build.SecurityContext
	if bsc == nil || bsc.RunAsUser == nil || *bsc.RunAsUser != 0 ||
		bsc.RunAsNonRoot == nil || *bsc.RunAsNonRoot ||
		bsc.AllowPrivilegeEscalation == nil || *bsc.AllowPrivilegeEscalation ||
		len(bsc.Capabilities.Drop) != 1 || bsc.Capabilities.Drop[0] != "ALL" ||
		len(bsc.Capabilities.Add) != 3 || bsc.Capabilities.Add[0] != "CHOWN" ||
		bsc.Capabilities.Add[1] != "DAC_OVERRIDE" || bsc.Capabilities.Add[2] != "FOWNER" {
		t.Errorf("build securityContext (baseline PSA: root, AE=false, CHOWN+DAC_OVERRIDE+FOWNER) = %+v", bsc)
	}
	if bsc.ReadOnlyRootFilesystem != nil && *bsc.ReadOnlyRootFilesystem {
		t.Errorf("readOnlyRootFilesystem must stay unset: kaniko unpacks into the container rootfs")
	}

	wantSpec, err := imageBuildConfig(validCfg())
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"--context=dir://" + workspaceMount,
		"--dockerfile=Dockerfile",
		"--label=" + configLabelKey + "=" + wantSpec.contentKey(),
		"--destination=" + testImage + ":" + testSHA,
		"--digest-file=" + terminationLog,
	}
	if len(build.Args) != len(wantArgs) {
		t.Fatalf("build args = %v, want %v", build.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if build.Args[i] != want {
			t.Errorf("build args[%d] = %q, want %q", i, build.Args[i], want)
		}
	}
	joined := strings.Join(build.Args, " ")
	for _, banned := range []string{"buildctl", "metadata-file", "sed ", "--no-push", "sh -c"} {
		if strings.Contains(joined, banned) {
			t.Errorf("build args must not contain %q: %v", banned, build.Args)
		}
	}
	if len(build.VolumeMounts) != 2 {
		t.Fatalf("build mounts = %v", build.VolumeMounts)
	}
	if build.VolumeMounts[1].Name != dockerCfgVolume || build.VolumeMounts[1].MountPath != dockerCfgMount ||
		!build.VolumeMounts[1].ReadOnly {
		t.Errorf("docker config mount = %+v", build.VolumeMounts[1])
	}

	if len(pod.Volumes) != 2 {
		t.Fatalf("volumes = %v", pod.Volumes)
	}
	ws := pod.Volumes[0]
	if ws.Name != workspaceVolume || ws.EmptyDir == nil {
		t.Errorf("workspace volume = %+v", ws)
	}
	secretVol := pod.Volumes[1]
	if secretVol.Name != dockerCfgVolume || secretVol.Secret == nil ||
		secretVol.Secret.SecretName != testRegistry || len(secretVol.Secret.Items) != 1 ||
		secretVol.Secret.Items[0].Key != ".dockerconfigjson" || secretVol.Secret.Items[0].Path != "config.json" {
		t.Errorf("docker config volume = %+v", secretVol.Secret)
	}
}

func TestImageBuild_JobSpecOverrides(t *testing.T) {
	cfg := validCfg()
	cfg["dockerfile"] = "sub/Dockerfile"
	cfg["context"] = "app"
	cfg["target"] = "prod"
	cfg["platform"] = "linux/amd64"
	cfg["args"] = map[string]any{"Z": "9", "A": "1"}
	cfg["namespace"] = "custom-ns"
	cfg["gitSecret"] = "git-pat"
	cfg["timeout"] = "5m"
	cfg["resources"] = map[string]any{"limits": map[string]any{"cpu": "3"}}

	var job *batchv1.Job
	client := captureJobClient(t, testDigest, &job)
	if _, err := (ImageBuild{}).RunCluster(context.Background(), cfg, engine.Inputs{}, fastRunner(client)); err != nil {
		t.Fatalf("RunCluster: %v", err)
	}
	if job.Namespace != "custom-ns" {
		t.Errorf("namespace = %s", job.Namespace)
	}
	if *job.Spec.ActiveDeadlineSeconds != 360 {
		t.Errorf("activeDeadline = %d, want 360 (5m+60s)", *job.Spec.ActiveDeadlineSeconds)
	}
	pod := job.Spec.Template.Spec

	init := pod.InitContainers[0]
	if len(init.VolumeMounts) != 2 || init.VolumeMounts[1].Name != gitAuthVolume ||
		init.VolumeMounts[1].MountPath != gitAuthMount || !init.VolumeMounts[1].ReadOnly {
		t.Errorf("init mounts = %v", init.VolumeMounts)
	}
	build := pod.Containers[0]
	if len(build.VolumeMounts) != 2 {
		t.Errorf("build must not mount the git secret: %v", build.VolumeMounts)
	}
	if build.Resources.Limits.Cpu().String() != "3" ||
		build.Resources.Requests.Cpu().String() != "1" {
		t.Errorf("resources overlay = %v (requests must stay defaulted)", build.Resources)
	}

	var gitVolume *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == gitAuthVolume {
			gitVolume = &pod.Volumes[i]
		}
	}
	if gitVolume == nil || gitVolume.Secret == nil || gitVolume.Secret.SecretName != "git-pat" ||
		len(gitVolume.Secret.Items) != 1 || gitVolume.Secret.Items[0].Key != "credential" ||
		gitVolume.Secret.Items[0].Path != "credential" {
		t.Errorf("git secret volume = %+v", gitVolume)
	}

	initScript := init.Args[0]
	if !strings.Contains(initScript, "cd "+shQuote(workspaceMount)) {
		t.Errorf("init script should cd into the shared checkout: %s", initScript)
	}
	buildArgs := build.Args
	joined := strings.Join(buildArgs, " ")
	ovSpec, err := imageBuildConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--context=dir://" + workspaceMount,
		"--context-sub-path=app",
		"--dockerfile=sub/Dockerfile",
		"--target=prod",
		"--custom-platform=linux/amd64",
		"--build-arg=A=1",
		"--build-arg=Z=9",
		"--label=" + configLabelKey + "=" + ovSpec.contentKey(),
		"--destination=" + testImage + ":" + testSHA,
		"--digest-file=" + terminationLog,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("build args missing %q:\n%v", want, buildArgs)
		}
	}
	iA := strings.Index(joined, "--build-arg=A=1")
	iZ := strings.Index(joined, "--build-arg=Z=9")
	if iA < 0 || iZ < 0 || iA > iZ {
		t.Errorf("build args must be sorted A before Z:\n%v", buildArgs)
	}
	if build.Command != nil {
		t.Errorf("build command = %v, want nil (no shell wrapper)", build.Command)
	}
}

func TestImageBuild_ShQuoteKeepsInjectionQuoted(t *testing.T) {
	nasty := "https://x.example/a'b;$(touch pwned).git"
	cfg := validCfg()
	cfg["repo"] = nasty
	cfg["sha"] = "deadbeef; rm -rf /"
	cfg["args"] = map[string]any{"K": "v'; echo pwned"}

	var job *batchv1.Job
	client := captureJobClient(t, testDigest, &job)
	if _, err := (ImageBuild{}).RunCluster(context.Background(), cfg, engine.Inputs{}, fastRunner(client)); err != nil {
		t.Fatalf("RunCluster: %v", err)
	}
	pod := job.Spec.Template.Spec
	initScript := pod.InitContainers[0].Args[0]
	wantRepo := shQuote(nasty)
	if !strings.Contains(initScript, wantRepo) {
		t.Errorf("repo not quoted (%s): %s", wantRepo, initScript)
	}
	if strings.Contains(initScript, "';$(touch pwned).git'") {
		t.Errorf("raw repo tail leaked unquoted: %s", initScript)
	}
	if !strings.Contains(initScript, shQuote("deadbeef; rm -rf /")) {
		t.Errorf("sha not quoted: %s", initScript)
	}
	// The kaniko side has no shell: the nasty build-arg value must
	// arrive verbatim as one argv element (no quoting needed, nothing
	// to inject into).
	build := pod.Containers[0]
	if build.Command != nil {
		t.Errorf("build command = %v, want nil (no shell to inject into)", build.Command)
	}
	found := false
	for _, a := range build.Args {
		if a == "--build-arg=K=v'; echo pwned" {
			found = true
		}
	}
	if !found {
		t.Errorf("build arg must pass verbatim as argv: %v", build.Args)
	}
}

func TestImageBuild_RunClusterTimeoutWrapsDeadline(t *testing.T) {
	cfg := validCfg()
	cfg["timeout"] = "50ms"
	client := onJobCreate(t, nil) // job created, pod never appears
	_, err := (ImageBuild{}).RunCluster(context.Background(), cfg, engine.Inputs{}, fastRunner(client))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err must wrap DeadlineExceeded: %v", err)
	}
	if !strings.Contains(err.Error(), "image-build:") {
		t.Errorf("err must carry the node prefix: %v", err)
	}
	remaining, lerr := client.BatchV1().Jobs(testNS).List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(remaining.Items) != 0 {
		t.Errorf("timed-out job must be deleted, found %d", len(remaining.Items))
	}
}

func TestImageBuild_RunClusterUnresolvedMarkerFailsEarly(t *testing.T) {
	cfg := validCfg()
	cfg["sha"] = engine.Reference{}
	_, err := (ImageBuild{}).RunCluster(context.Background(), cfg, engine.Inputs{}, fastRunner(fake.NewClientset()))
	if err == nil || !strings.Contains(err.Error(), "sha must resolve to a non-empty string") {
		t.Errorf("err = %v", err)
	}
}

func del(cfg map[string]any, key string) map[string]any {
	delete(cfg, key)
	return cfg
}

func set(cfg map[string]any, key string, val any) map[string]any {
	cfg[key] = val
	return cfg
}
