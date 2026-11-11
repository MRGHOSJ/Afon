package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/engine"
)

// fakeSleep is a test-only action node: it signals started, then blocks
// until the run's context is cancelled. It never ships in the CLI's real
// registry — tests inject it through extraNodeTypes.
type fakeSleep struct {
	started chan struct{}
	once    sync.Once
}

func (f *fakeSleep) Type() string              { return "fake-sleep" }
func (f *fakeSleep) TypeVersion() string       { return "1" }
func (f *fakeSleep) Kind() engine.Kind         { return engine.KindAction }
func (f *fakeSleep) Validate(cfg any) error    { return nil }
func (f *fakeSleep) DeclaredOutputs() []string { return nil }

func (f *fakeSleep) Run(ctx context.Context, cfg any, in engine.Inputs) (engine.Outputs, error) {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

const cancelFlow = `apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: cancel}
spec:
  nodes:
    - id: sleep
      type: fake-sleep
      config: {}
    - id: tail
      type: fake-sleep
      needs: [sleep]
      config: {}
`

// TestApply_CancellationInProcess drives the real CLI entrypoint with a
// cancelled context — the same path Ctrl-C takes (notifyContext → runApply
// → engine.Apply) — without sending a signal to the test process. Asserts
// Execute returns an error (main exits 1), both nodes report cancelled
// and never failed or skipped, and the fake-driver banner stays on stderr.
func TestApply_CancellationInProcess(t *testing.T) {
	node := &fakeSleep{started: make(chan struct{})}
	oldExtra := extraNodeTypes
	extraNodeTypes = []engine.NodeType{node}
	t.Cleanup(func() { extraNodeTypes = oldExtra })

	dir := t.TempDir()
	flowPath := filepath.Join(dir, "flow.yaml")
	if err := os.WriteFile(flowPath, []byte(cancelFlow), 0o644); err != nil {
		t.Fatal(err)
	}

	// Inject a cancellable context through the notifyContext seam: the
	// test cancels it exactly where a SIGINT would.
	ctx, cancel := context.WithCancel(context.Background())
	oldNotify := notifyContext
	notifyContext = func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
		return ctx, cancel
	}
	t.Cleanup(func() { notifyContext = oldNotify; cancel() })

	// Swap os.Stdout/os.Stderr for pipes so the captured output is what
	// a terminal would show.
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		wOut.Close()
		wErr.Close()
		rOut.Close()
		rErr.Close()
	})

	rootCmd.SetArgs([]string{
		"apply", "-f", flowPath, "--state-dir", dir, "-o", "text", "--parallelism", "1",
	})

	done := make(chan error, 1)
	go func() { done <- Execute() }()

	// Wait until the fake-sleep node is actually running, then cancel —
	// the in-flight node and the never-scheduled dependent must both
	// report cancelled.
	select {
	case <-node.started:
	case execErr := <-done:
		t.Fatalf("Execute returned before fake-sleep started: %v", execErr)
	case <-time.After(10 * time.Second):
		t.Fatal("fake-sleep node never started")
	}
	cancel()

	var execErr error
	select {
	case execErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return after cancellation")
	}
	if execErr == nil {
		t.Fatal("Execute must return an error on cancellation (main exits 1)")
	}
	if !errors.Is(execErr, context.Canceled) {
		t.Errorf("Execute error = %v, want context.Canceled", execErr)
	}

	// Writers are done: close them so ReadAll sees EOF, then restore the
	// real streams and read what was captured.
	os.Stdout, os.Stderr = oldOut, oldErr
	wOut.Close()
	wErr.Close()
	stdout, err := io.ReadAll(rOut)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(rErr)
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := string(stdout), string(stderr)

	// Exit-code contract: main turns a non-nil Execute error into 1; the
	// error line lands on stderr.
	if !strings.Contains(errOut, "error:") {
		t.Errorf("stderr must carry the error line:\n%s", errOut)
	}
	// Results print even on the error path, all cancelled — never failed
	// (engine rule: cancellation is not failure) and never skipped (the
	// caller's context ended, not a dependency failure).
	if !strings.Contains(out, "apply result:") {
		t.Errorf("stdout must show apply results on cancellation:\n%s", out)
	}
	if n := strings.Count(out, "cancelled"); n < 2 {
		t.Errorf("want both nodes cancelled, got %d in:\n%s", n, out)
	}
	if strings.Contains(out, "failed") {
		t.Errorf("cancellation must not report failed:\n%s", out)
	}
	if strings.Contains(out, "skipped") {
		t.Errorf("cancelled run must not report skipped:\n%s", out)
	}
	// The fake-driver banner stays on stderr, never stdout.
	if !strings.Contains(errOut, "fake driver: no cluster is touched") {
		t.Errorf("stderr must carry the fake-driver banner:\n%s", errOut)
	}
	if strings.Contains(out, "fake driver: no cluster is touched") {
		t.Errorf("banner leaked to stdout:\n%s", out)
	}
}
