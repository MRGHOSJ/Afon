package k8s

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// newReleaseBackend wires a SecretBackend over a fake cluster with the
// secrets list kind registered.
func newReleaseBackend(t *testing.T, flow string) (*SecretBackend, *fake.FakeDynamicClient) {
	t.Helper()
	listKinds := map[schema.GroupVersionResource]string{secretsGVR: "SecretList"}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	b, err := NewSecretBackend(dyn, testNS, flow)
	if err != nil {
		t.Fatalf("NewSecretBackend: %v", err)
	}
	return b, dyn
}

// seedRec builds a promotable record for direct insertion.
func seedRec(seq int, status release.Status, started time.Time) release.Record {
	rec := release.Record{
		APIVersion: release.APIVersion,
		Flow:       testFlow,
		Sequence:   seq,
		Status:     status,
		Trigger:    release.TriggerManual,
		StartedAt:  started,
	}
	if status != release.StatusPending {
		rec.FinishedAt = started.Add(time.Second)
	}
	return rec
}

// seed inserts a record without Start's concurrency bookkeeping —
// tests need exact sequences and (stale) timestamps.
func seed(t *testing.T, b *SecretBackend, rec release.Record) {
	t.Helper()
	if err := b.create(context.Background(), rec); err != nil {
		t.Fatalf("seed v%d: %v", rec.Sequence, err)
	}
}

func TestSecretBackendStartCreatesPending(t *testing.T) {
	b, dyn := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	started, err := b.Start(ctx, release.Record{Flow: testFlow, Trigger: release.TriggerManual}, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Sequence != 1 || started.PreviousSequence != 0 {
		t.Fatalf("seq = %d prev = %d, want 1/0", started.Sequence, started.PreviousSequence)
	}
	if started.Status != release.StatusPending {
		t.Fatalf("status = %q, want pending", started.Status)
	}
	if started.StartedAt.IsZero() {
		t.Fatal("StartedAt not set by Start")
	}
	if !started.StartedAt.Before(time.Now().Add(time.Second)) {
		t.Fatalf("StartedAt in the future: %v", started.StartedAt)
	}

	// The Secret exists with the naming and labels the plan locked in.
	sec, err := dyn.Resource(secretsGVR).Namespace(testNS).
		Get(ctx, releaseSecretName(testFlow, 1), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if got := sec.GetLabels()[releaseFlowLabel]; got != testFlow {
		t.Errorf("flow label = %q, want %q", got, testFlow)
	}
	if got := sec.GetLabels()[releaseSeqLabel]; got != "1" {
		t.Errorf("seq label = %q, want 1", got)
	}

	// Get/Latest read it back.
	got, ok, err := b.Get(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("Get(1): ok=%v err=%v", ok, err)
	}
	if got.Status != release.StatusPending || got.Trigger != release.TriggerManual {
		t.Fatalf("Get(1) = %+v", got)
	}
	latest, ok, err := b.Latest(ctx)
	if err != nil || !ok || latest.Sequence != 1 {
		t.Fatalf("Latest: seq=%d ok=%v err=%v", latest.Sequence, ok, err)
	}
}

func TestSecretBackendSequenceMonotonic(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	promote := func(rec release.Record) {
		t.Helper()
		rec.Status = release.StatusSucceeded
		if err := b.Promote(ctx, rec); err != nil {
			t.Fatalf("Promote v%d: %v", rec.Sequence, err)
		}
	}
	r1, err := b.Start(ctx, release.Record{Flow: testFlow}, false)
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	promote(r1)
	r2, err := b.Start(ctx, release.Record{Flow: testFlow}, false)
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if r2.PreviousSequence != 1 {
		t.Fatalf("v2 prev = %d, want 1", r2.PreviousSequence)
	}
	promote(r2)
	r3, err := b.Start(ctx, release.Record{Flow: testFlow}, false)
	if err != nil {
		t.Fatalf("Start 3: %v", err)
	}
	if r3.Sequence != 3 || r3.PreviousSequence != 2 {
		t.Fatalf("v3 seq/prev = %d/%d, want 3/2", r3.Sequence, r3.PreviousSequence)
	}
}

func TestSecretBackendFreshPendingBlocks(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	if _, err := b.Start(ctx, release.Record{Flow: testFlow}, false); err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	// Fresh pending blocks even with takeover: the window has not
	// elapsed, so the first apply is presumed alive.
	for _, takeover := range []bool{false, true} {
		_, err := b.Start(ctx, release.Record{Flow: testFlow}, takeover)
		conc, ok := err.(*release.ErrConcurrentApply)
		if !ok {
			t.Fatalf("takeover=%v: err = %v, want *ErrConcurrentApply", takeover, err)
		}
		if conc.Stale {
			t.Errorf("takeover=%v: Stale = true for a fresh pending record", takeover)
		}
		if conc.Sequence != 1 || conc.Flow != testFlow {
			t.Errorf("takeover=%v: err = %+v, want flow/seq web/1", takeover, conc)
		}
		if msg := conc.Error(); !strings.Contains(msg, "another apply is in progress") {
			t.Errorf("fresh refusal message: %q", msg)
		}
	}
}

func TestSecretBackendStalePendingBlocksWithoutTakeover(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	staleAt := time.Now().Add(-release.PendingStaleAfter - time.Minute)
	seed(t, b, seedRec(1, release.StatusPending, staleAt))

	_, err := b.Start(ctx, release.Record{Flow: testFlow}, false)
	conc, ok := err.(*release.ErrConcurrentApply)
	if !ok {
		t.Fatalf("err = %v, want *ErrConcurrentApply", err)
	}
	if !conc.Stale {
		t.Fatal("Stale = false for a pending record past PendingStaleAfter")
	}
	msg := conc.Error()
	if !strings.Contains(msg, "probable crashed run") || !strings.Contains(msg, "--takeover") {
		t.Fatalf("stale refusal message: %q", msg)
	}
}

func TestSecretBackendTakeoverSupersedesStale(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	staleAt := time.Now().Add(-release.PendingStaleAfter - time.Minute)
	seed(t, b, seedRec(1, release.StatusPending, staleAt))

	next, err := b.Start(ctx, release.Record{Flow: testFlow}, true)
	if err != nil {
		t.Fatalf("Start with takeover: %v", err)
	}
	if next.Sequence != 2 || next.PreviousSequence != 1 {
		t.Fatalf("seq/prev = %d/%d, want 2/1", next.Sequence, next.PreviousSequence)
	}

	old, ok, err := b.Get(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("Get(1): ok=%v err=%v", ok, err)
	}
	if old.Status != release.StatusFailed {
		t.Fatalf("superseded status = %q, want failed", old.Status)
	}
	if !strings.Contains(old.Error, "superseded by --takeover") {
		t.Fatalf("superseded error = %q", old.Error)
	}
	if old.FinishedAt.IsZero() {
		t.Fatal("superseded record has no FinishedAt")
	}
}

func TestSecretBackendKeepCount(t *testing.T) {
	b, dyn := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	base := time.Now().Add(-time.Hour)
	for seq := 1; seq <= 12; seq++ {
		seed(t, b, seedRec(seq, release.StatusSucceeded, base.Add(time.Duration(seq)*time.Minute)))
	}
	rec, err := b.Start(ctx, release.Record{Flow: testFlow}, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rec.Sequence != 13 {
		t.Fatalf("seq = %d, want 13", rec.Sequence)
	}
	rec.Status = release.StatusSucceeded
	if err := b.Promote(ctx, rec); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	records, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != release.KeepCount {
		t.Fatalf("kept %d records, want %d", len(records), release.KeepCount)
	}
	// Newest survive: v13..v4 remain, v3..v1 pruned.
	if records[0].Sequence != 13 || records[len(records)-1].Sequence != 4 {
		t.Fatalf("kept seqs %d..%d, want 13..4", records[0].Sequence, records[len(records)-1].Sequence)
	}
	list, err := dyn.Resource(secretsGVR).Namespace(testNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List secrets: %v", err)
	}
	if len(list.Items) != release.KeepCount {
		t.Fatalf("%d secrets remain, want %d", len(list.Items), release.KeepCount)
	}
}

func TestSecretBackendPromotePendingRefused(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	rec, err := b.Start(ctx, release.Record{Flow: testFlow}, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = b.Promote(ctx, rec)
	if err == nil || !strings.Contains(err.Error(), "cannot promote a pending record") {
		t.Fatalf("Promote(pending): err = %v", err)
	}
}

func TestSecretBackendPromoteRoundTrip(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	rec, err := b.Start(ctx, release.Record{Flow: testFlow, Trigger: release.TriggerRollback}, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec.Status = release.StatusRolledBack
	rec.Manifests = []release.ManifestStream{{Node: "r1", YAML: "kind: ConfigMap\n"}}
	rec.Inventory = []release.InventoryObject{
		{Version: "v1", Kind: "ConfigMap", Namespace: testNS, Name: "a", Node: "r1", UID: "u1", Hash: "h1"},
	}
	if err := b.Promote(ctx, rec); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	got, ok, err := b.Get(ctx, rec.Sequence)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Status != release.StatusRolledBack || got.Trigger != release.TriggerRollback {
		t.Fatalf("status/trigger = %q/%q", got.Status, got.Trigger)
	}
	if got.FinishedAt.IsZero() {
		t.Fatal("Promote did not fill FinishedAt")
	}
	if len(got.Manifests) != 1 || got.Manifests[0].Node != "r1" {
		t.Fatalf("manifests = %+v", got.Manifests)
	}
	if len(got.Inventory) != 1 || got.Inventory[0].UID != "u1" {
		t.Fatalf("inventory = %+v", got.Inventory)
	}
}

func TestSecretBackendEmptyFlow(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	if _, ok, err := b.Latest(ctx); err != nil || ok {
		t.Fatalf("Latest on empty flow: ok=%v err=%v", ok, err)
	}
	if _, ok, err := b.Get(ctx, 1); err != nil || ok {
		t.Fatalf("Get on empty flow: ok=%v err=%v", ok, err)
	}
	if _, _, err := b.Get(ctx, 0); err == nil {
		t.Fatal("Get(0): want error, got nil")
	}
}

func TestSecretBackendFlowMismatch(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	_, err := b.Start(context.Background(), release.Record{Flow: "other"}, false)
	if err == nil || !strings.Contains(err.Error(), "flow") {
		t.Fatalf("Start with wrong flow: err = %v", err)
	}
}

func TestNewSecretBackendValidation(t *testing.T) {
	listKinds := map[schema.GroupVersionResource]string{secretsGVR: "SecretList"}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	cases := []struct {
		flow    string
		wantErr bool
	}{
		{"", true},            // required
		{"has space", true},   // invalid label value
		{"-leading", true},    // invalid label value and DNS name
		{"UPPER-case", true},  // label-ok, but the Secret name must be DNS
		{"under_score", true}, // label-ok, but the Secret name must be DNS
		{"a.b", false},        // dots satisfy both label and DNS rules
		{"ok-flow", false},
	}
	for _, tc := range cases {
		_, err := NewSecretBackend(dyn, testNS, tc.flow)
		if tc.wantErr && err == nil {
			t.Errorf("flow %q: want validation error, got nil", tc.flow)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("flow %q: unexpected error %v", tc.flow, err)
		}
	}
}

func TestSecretBackendCorruptRecord(t *testing.T) {
	b, dyn := newReleaseBackend(t, testFlow)
	ctx := context.Background()

	// A hand-edited Secret with undecodable payload must surface, not
	// silently vanish from the listing.
	sec := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": releaseSecretName(testFlow, 1), "namespace": testNS,
			"labels": map[string]any{releaseFlowLabel: testFlow, releaseSeqLabel: "1"},
		},
		"data": map[string]any{releaseDataKey: base64.StdEncoding.EncodeToString([]byte("{not json"))},
	}}
	if _, err := dyn.Resource(secretsGVR).Namespace(testNS).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create corrupt secret: %v", err)
	}
	_, err := b.List(ctx)
	if err == nil || !strings.Contains(err.Error(), "corrupt") || !strings.Contains(err.Error(), "delete it to recover") {
		t.Fatalf("List with corrupt record: err = %v", err)
	}
}

func TestSecretBackendListNewestFirst(t *testing.T) {
	b, _ := newReleaseBackend(t, testFlow)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	seed(t, b, seedRec(1, release.StatusSucceeded, base))
	seed(t, b, seedRec(3, release.StatusSucceeded, base.Add(2*time.Minute)))
	seed(t, b, seedRec(2, release.StatusFailed, base.Add(time.Minute)))

	records, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []int{3, 2, 1}
	if len(records) != len(want) {
		t.Fatalf("got %d records, want %d", len(records), len(want))
	}
	for i, seq := range want {
		if records[i].Sequence != seq {
			t.Fatalf("records[%d].Sequence = %d, want %d", i, records[i].Sequence, seq)
		}
	}
}
