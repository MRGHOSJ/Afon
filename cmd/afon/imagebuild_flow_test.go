package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/build"
	"github.com/MRGHOSJ/afon/pkg/driver/fake"
	"github.com/MRGHOSJ/afon/pkg/engine"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestFlow_ImageBuildGitToDigest is the Stage 7 integration path:
// file:// git-source → image-build (Job on an injected fake cluster
// returning a digest) → app-manifests consuming ${build.ref}, all
// through the CLI's own registry (parseFlow) — so it fails until
// newRegistry registers the Stage 7 nodes. A second apply proves the
// cache short-circuit: a poison runner whose client errors on any
// call sees zero invocations.
func TestFlow_ImageBuildGitToDigest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	const testDigest = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const wantRef = "127.0.0.1:1/example/app@" + testDigest

	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		full := append([]string{"-C", repo}, args...)
		out, err := exec.Command("git", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.email", "afon-test@example.invalid")
	git("config", "user.name", "Afon Test")
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "init")
	sha := git("rev-parse", "HEAD")

	p := filepath.ToSlash(repo)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	flowPath := filepath.Join(dir, "flow.yaml")
	flow := `apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: imgflow
spec:
  target:
    namespace: afon-demo
  nodes:
    - id: src
      type: git-source
      config:
        url: file://` + p + `
        ref: main
    - id: build
      type: image-build
      config:
        image: 127.0.0.1:1/example/app
        repo: ${src.url}
        sha: ${src.sha}
        registrySecret: dockerhub-creds
    - id: web
      type: app-manifests
      config:
        name: hello
        image: ${build.ref}
`
	if err := os.WriteFile(flowPath, []byte(flow), 0o644); err != nil {
		t.Fatal(err)
	}

	// The CLI's own compile path: registration lives or dies here.
	cf, err := parseFlow(flowPath)
	if err != nil {
		t.Fatalf("parseFlow (registry wiring): %v", err)
	}
	store, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := fake.NewPersistent(filepath.Join(dir, ".afon", "fake-state.json"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Plan predicts work for every node (git-source may already know
	// its inputs; downstream nodes wait on outputs).
	pres, err := engine.BuildPlanOpts(ctx, cf, store, d, engine.PlanOptions{})
	if err != nil {
		t.Fatalf("BuildPlanOpts: %v", err)
	}
	if got := strings.Count(pres.RenderText(), "will-run"); got < 2 {
		t.Errorf("plan should predict will-run for the unresolved chain, got %d\n%s", got, pres.RenderText())
	}

	// Fake cluster: the Job lands, a succeeded pod injects the digest
	// on the build container's termination message.
	clientset := k8sfake.NewClientset()
	clientset.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		if err := clientset.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), job, job.Namespace); err != nil {
			return true, nil, err
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "imagebuild-pod",
				Namespace: job.Namespace,
				Labels:    map[string]string{build.RunIDLabel: job.Labels[build.RunIDLabel]},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: build.BuildContainerName,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: testDigest},
					},
				}},
			},
		}
		if err := clientset.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
			return true, nil, err
		}
		return true, job, nil
	})

	res, err := engine.Apply(ctx, cf, store, d, engine.ExecutorOptions{
		Parallelism: 4,
		Cluster:     build.NewRunner(clientset),
	})
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, renderRunResult(res))
	}
	for _, id := range []string{"src", "build", "web"} {
		r, ok := res.Get(id)
		if !ok || r.Status != engine.StatusSucceeded {
			t.Errorf("node %q status=%v err=%q", id, r.Status, r.ErrText)
		}
	}
	if r, ok := res.Get("src"); !ok || !r.Outputs["sha"].Known || r.Outputs["sha"].Data != sha {
		t.Errorf("src sha output = %+v, want %s", r.Outputs["sha"], sha)
	}
	bres, _ := res.Get("build")
	if v := bres.Outputs["ref"]; !v.Known || v.Data != wantRef {
		t.Errorf("build ref output = %+v, want %s", v, wantRef)
	}
	if v := bres.Outputs["digest"]; !v.Known || v.Data != testDigest {
		t.Errorf("build digest output = %+v, want %s", v, testDigest)
	}
	wres, _ := res.Get("web")
	rendered := wres.Outputs["rendered"]
	str, isStr := rendered.Data.(string)
	if !rendered.Known || !isStr || !strings.Contains(str, "image: "+wantRef) {
		t.Errorf("web rendered must deploy %s, got:\n%v", wantRef, rendered.Data)
	}

	// The throwaway Job is gone after a successful run (ttl is only
	// the crash net).
	jobs, err := clientset.BatchV1().Jobs(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("build jobs must always be deleted, found %d", len(jobs.Items))
	}

	// Second apply: everything cached; the poison cluster client must
	// see zero calls — a cache hit never touches the cluster.
	var poisonCalls atomic.Int32
	poison := k8sfake.NewClientset()
	poison.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		poisonCalls.Add(1)
		return true, nil, context.DeadlineExceeded
	})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	res2, err := engine.Apply(ctx2, cf, store, d, engine.ExecutorOptions{
		Parallelism: 4,
		Cluster:     build.NewRunner(poison),
	})
	if err != nil {
		t.Fatalf("cached Apply: %v\n%s", err, renderRunResult(res2))
	}
	for _, id := range []string{"src", "build", "web"} {
		r, ok := res2.Get(id)
		if !ok || !r.Cached {
			t.Errorf("second apply node %q: cached=%v status=%v", id, r.Cached, r.Status)
		}
	}
	if n := poisonCalls.Load(); n != 0 {
		t.Errorf("cache hits must never touch the cluster, poison client called %d times", n)
	}
}
