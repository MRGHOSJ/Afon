package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver/fake"
	"github.com/MRGHOSJ/afon/pkg/engine"
	"github.com/MRGHOSJ/afon/pkg/release"
	"github.com/MRGHOSJ/afon/pkg/schema"
)

// TestRollbackRequiresK8sDriver pins the shared refusal: with the
// default fake driver (or any non-k8s driver) rollback refuses with
// the locked message instead of pretending it replayed something.
func TestRollbackRequiresK8sDriver(t *testing.T) {
	for _, driverFlag := range []string{"fake", "", "bogus"} {
		t.Run("driver_"+driverFlag, func(t *testing.T) {
			old := flagDriver
			flagDriver = driverFlag
			t.Cleanup(func() { flagDriver = old })

			err := runRollback(context.Background())
			if err == nil {
				t.Fatal("runRollback: want error, got nil")
			}
			want := "release records require the k8s driver (pass --driver k8s --context ...)"
			if err.Error() != want {
				t.Fatalf("err = %q, want %q", err, want)
			}
		})
	}
}

// failAction is a node type that always fails — the auto-rollback
// trigger in TestMaybeAutoRollback.
type failAction struct{}

func (failAction) Type() string           { return "fail-action" }
func (failAction) TypeVersion() string    { return "1" }
func (failAction) Kind() engine.Kind      { return engine.KindAction }
func (failAction) Validate(cfg any) error { return nil }
func (failAction) DeclaredOutputs() []string {
	return nil
}
func (failAction) Run(context.Context, any, engine.Inputs) (engine.Outputs, error) {
	return nil, errors.New("deliberate failure")
}

// stubReleaseBackend is an in-memory release.Backend: sequences in
// order, Promote replaces the matching record, List newest-first.
type stubReleaseBackend struct {
	mu      sync.Mutex
	nextSeq int
	records []release.Record // oldest first
}

func (b *stubReleaseBackend) seed(rec release.Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records = append(b.records, rec)
	if rec.Sequence > b.nextSeq {
		b.nextSeq = rec.Sequence
	}
}

func (b *stubReleaseBackend) Start(_ context.Context, rec release.Record, _ bool) (release.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextSeq++
	rec.APIVersion = release.APIVersion
	rec.Sequence = b.nextSeq
	rec.Status = release.StatusPending
	rec.StartedAt = time.Now()
	b.records = append(b.records, rec)
	return rec, nil
}

func (b *stubReleaseBackend) Promote(_ context.Context, rec release.Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec.FinishedAt.IsZero() {
		rec.FinishedAt = time.Now()
	}
	for i := range b.records {
		if b.records[i].Sequence == rec.Sequence {
			b.records[i] = rec
			return nil
		}
	}
	b.records = append(b.records, rec)
	return nil
}

func (b *stubReleaseBackend) List(_ context.Context) ([]release.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]release.Record, len(b.records))
	copy(out, b.records)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (b *stubReleaseBackend) Latest(ctx context.Context) (release.Record, bool, error) {
	out, err := b.List(ctx)
	if err != nil || len(out) == 0 {
		return release.Record{}, false, err
	}
	return out[0], true, nil
}

func (b *stubReleaseBackend) Get(_ context.Context, seq int) (release.Record, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.records {
		if r.Sequence == seq {
			return r, true, nil
		}
	}
	return release.Record{}, false, nil
}

func (b *stubReleaseBackend) find(seq int) (release.Record, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.records {
		if r.Sequence == seq {
			return r, true
		}
	}
	return release.Record{}, false
}

// TestMaybeAutoRollback drives the full auto path the apply entrypoint
// wires: a flow whose only node fails with onFailure: rollback, armed
// against a backend whose v1 succeeded — the notice hits stderr, v1's
// manifest replays through the fake driver with NO confirmation
// prompt, and a v3 rollback record lands on the backend.
func TestMaybeAutoRollback(t *testing.T) {
	oldOut, oldWait := flagOutput, flagNoWait
	flagOutput, flagNoWait = "text", true
	t.Cleanup(func() { flagOutput, flagNoWait = oldOut, oldWait })

	reg, err := newRegistry(failAction{})
	if err != nil {
		t.Fatal(err)
	}
	flow, err := schema.ParseBytes([]byte(`apiVersion: afon.dev/flow/v1
kind: Flow
metadata: {name: auto}
spec:
  nodes:
    - id: a
      type: fail-action
      onFailure: rollback
`), "flow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cf, err := engine.Compile(flow, reg)
	if err != nil {
		t.Fatal(err)
	}

	backend := &stubReleaseBackend{}
	backend.seed(release.Record{
		APIVersion: release.APIVersion,
		Flow:       "auto",
		Sequence:   1,
		Status:     release.StatusSucceeded,
		Trigger:    release.TriggerManual,
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Manifests: []release.ManifestStream{{
			Node: "a",
			YAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: demo\n",
		}},
	})

	d := fake.New()
	store := engine.NewMemoryStore()

	// The failed apply: writes v2 (failed) onto the backend, exactly
	// as runApply's Apply does before it calls maybeAutoRollback.
	res, applyErr := engine.Apply(context.Background(), cf, store, d,
		engine.ExecutorOptions{Backend: backend})
	if applyErr == nil || !strings.Contains(applyErr.Error(), "deliberate failure") {
		t.Fatalf("apply err = %v, want the failed node", applyErr)
	}

	var stdout, stderr string
	stdout, stderr, err = captureStdoutStderr(t, func() error {
		return maybeAutoRollback(context.Background(), cf, store, d, backend, res)
	})
	if err != nil {
		t.Fatalf("maybeAutoRollback: %v", err)
	}
	if !strings.Contains(stderr, "rolling back to release 1") {
		t.Errorf("stderr missing the auto-rollback notice:\n%s", stderr)
	}
	rec, ok := backend.find(3) // v1 seed, v2 failed apply, v3 rollback
	if !ok {
		t.Fatalf("no v3 record")
	}
	if rec.Trigger != release.TriggerRollback {
		t.Errorf("trigger = %q, want rollback", rec.Trigger)
	}
	if rec.RolledBackTo != 1 {
		t.Errorf("rolledBackTo = %d, want 1", rec.RolledBackTo)
	}
	if rec.Status != release.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", rec.Status)
	}
	if !strings.Contains(stdout, "succeeded") {
		t.Errorf("stdout missing the rollback's results:\n%s", stdout)
	}
}
