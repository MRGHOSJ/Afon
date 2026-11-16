package k8s

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MRGHOSJ/afon/pkg/release"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

// secretsGVR is where release records live: one Secret per record, in
// the flow's target namespace.
var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

const (
	// releaseFlowLabel scopes every list/select to one flow's records;
	// releaseSeqLabel mirrors the sequence for cheap client-side
	// filtering and debugging.
	releaseFlowLabel = "afon.dev/release-flow"
	releaseSeqLabel  = "afon.dev/release-seq"
	// releaseDataKey is the Secret data key holding the JSON record.
	releaseDataKey = "release"
)

// errSeqRace reports a Create lost to a concurrent Start: the name is
// taken, so the caller re-lists and retries with a fresh sequence.
var errSeqRace = fmt.Errorf("release sequence already taken")

// releaseSecretName is the record's Secret name: one flow, one
// sequence — unambiguous even with dots in the flow name (the
// trailing .v<N> pins the parse).
func releaseSecretName(flow string, seq int) string {
	return fmt.Sprintf("afon.release.%s.v%d", flow, seq)
}

// SecretBackend stores release records as one Secret per record in the
// target namespace, keeping the newest release.KeepCount per flow.
// It is safe for concurrent use: every call goes through the
// thread-safe dynamic client, and sequence races resolve by retrying
// the whole reservation.
type SecretBackend struct {
	dyn       dynamic.Interface
	namespace string
	flow      string
}

// NewSecretBackend binds a record backend to one flow. The flow name
// becomes a label value and part of a Secret name, so it must satisfy
// both Kubernetes rules — checked here, before any cluster call.
func NewSecretBackend(dyn dynamic.Interface, namespace, flow string) (*SecretBackend, error) {
	if dyn == nil {
		return nil, fmt.Errorf("release backend: dynamic client is required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("release backend: namespace is required")
	}
	if flow == "" {
		return nil, fmt.Errorf("release backend: flow name is required")
	}
	if errs := validation.IsValidLabelValue(flow); len(errs) > 0 {
		return nil, fmt.Errorf("flow name %q cannot be stored in release records: %s",
			flow, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(releaseSecretName(flow, 1)); len(errs) > 0 {
		return nil, fmt.Errorf("flow name %q cannot be stored in release records: %s",
			flow, strings.Join(errs, "; "))
	}
	return &SecretBackend{dyn: dyn, namespace: namespace, flow: flow}, nil
}

// Backend returns a release.Backend sharing this driver's client and
// namespace, scoped to one flow.
func (d *Driver) Backend(flow string) (*SecretBackend, error) {
	return NewSecretBackend(d.dyn, d.namespace, flow)
}

// Start reserves the next sequence and writes the pending record
// (release.Backend). A pending record blocks: fresh ones always,
// stale ones unless takeover — which promotes the stale record to
// failed first. Create races re-list and retry.
func (b *SecretBackend) Start(ctx context.Context, rec release.Record, takeover bool) (release.Record, error) {
	if rec.Flow != b.flow {
		return rec, fmt.Errorf("release backend for flow %q received a record for flow %q", b.flow, rec.Flow)
	}
	for attempt := 0; attempt < 3; attempt++ {
		records, err := b.List(ctx)
		if err != nil {
			return rec, err
		}
		maxSeq := 0
		var pending *release.Record
		for i := range records { // newest first: the first pending wins
			r := records[i]
			if r.Sequence > maxSeq {
				maxSeq = r.Sequence
			}
			if r.Status == release.StatusPending && pending == nil {
				cp := r
				pending = &cp
			}
		}
		if pending != nil {
			stale := pending.IsStale(time.Now())
			if !takeover || !stale {
				return rec, &release.ErrConcurrentApply{
					Flow:      b.flow,
					Sequence:  pending.Sequence,
					StartedAt: pending.StartedAt,
					Stale:     stale,
				}
			}
			superseded := *pending
			superseded.Status = release.StatusFailed
			superseded.FinishedAt = time.Now()
			superseded.Error = fmt.Sprintf("superseded by --takeover: pending since %s",
				pending.StartedAt.Format(time.RFC3339))
			if err := b.write(ctx, superseded); err != nil {
				return rec, err
			}
		}
		next := rec
		next.APIVersion = release.APIVersion
		next.Sequence = maxSeq + 1
		next.PreviousSequence = maxSeq
		next.Status = release.StatusPending
		if next.Trigger == "" {
			next.Trigger = release.TriggerManual
		}
		next.StartedAt = time.Now()
		next.FinishedAt = time.Time{}
		next.Manifests = nil
		next.Inventory = nil
		next.Error = ""
		if err := next.Validate(); err != nil {
			return rec, err
		}
		if err := b.create(ctx, next); err != nil {
			if errors.Is(err, errSeqRace) {
				continue
			}
			return rec, err
		}
		return next, nil
	}
	return rec, fmt.Errorf("release records for flow %q: sequence contention did not settle after 3 attempts", b.flow)
}

// Promote finalizes a record Start returned and prunes beyond
// release.KeepCount. The record itself is written before pruning: a
// prune failure leaves the record durable and reports cleanup only.
func (b *SecretBackend) Promote(ctx context.Context, rec release.Record) error {
	if rec.Flow != b.flow {
		return fmt.Errorf("release backend for flow %q received a record for flow %q", b.flow, rec.Flow)
	}
	if rec.Status == release.StatusPending {
		return fmt.Errorf("release v%d: cannot promote a pending record", rec.Sequence)
	}
	if rec.FinishedAt.IsZero() {
		rec.FinishedAt = time.Now()
	}
	if err := rec.Validate(); err != nil {
		return err
	}
	if err := b.write(ctx, rec); err != nil {
		return err
	}
	return b.prune(ctx)
}

// Latest returns the highest-sequence record, or ok=false when the
// flow has none.
func (b *SecretBackend) Latest(ctx context.Context) (release.Record, bool, error) {
	records, err := b.List(ctx)
	if err != nil {
		return release.Record{}, false, err
	}
	if len(records) == 0 {
		return release.Record{}, false, nil
	}
	return records[0], true, nil
}

// List returns every record for the flow, newest first. A corrupt
// record fails the whole listing: hiding one would silently mislead
// every later rollback or status decision. The error names the Secret
// so the operator can delete it and recover.
func (b *SecretBackend) List(ctx context.Context) ([]release.Record, error) {
	list, err := b.dyn.Resource(secretsGVR).Namespace(b.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{releaseFlowLabel: b.flow}.String(),
	})
	if apierrors.IsNotFound(err) {
		// A missing namespace has no records — the fresh-cluster case.
		// (List only 404s when the namespace itself is gone; a namespace
		// that exists but has no matching Secrets returns an empty list.)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing release records for flow %q: %w", b.flow, err)
	}
	records := make([]release.Record, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		rec, err := decodeSecret(item)
		if err != nil {
			return nil, fmt.Errorf("secret %q: corrupt release record: %v (delete it to recover)", item.GetName(), err)
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Sequence > records[j].Sequence })
	return records, nil
}

// Get returns one record by sequence, or ok=false when absent.
func (b *SecretBackend) Get(ctx context.Context, seq int) (release.Record, bool, error) {
	if seq < 1 {
		return release.Record{}, false, fmt.Errorf("release sequence must be >= 1, got %d", seq)
	}
	sec, err := b.dyn.Resource(secretsGVR).Namespace(b.namespace).
		Get(ctx, releaseSecretName(b.flow, seq), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return release.Record{}, false, nil
	}
	if err != nil {
		return release.Record{}, false, fmt.Errorf("reading release v%d: %w", seq, err)
	}
	rec, err := decodeSecret(sec)
	if err != nil {
		return release.Record{}, false, fmt.Errorf("secret %q: corrupt release record: %v (delete it to recover)", sec.GetName(), err)
	}
	if rec.Sequence != seq {
		return release.Record{}, false, fmt.Errorf("secret %q holds release v%d (name and record disagree)", sec.GetName(), rec.Sequence)
	}
	return rec, true, nil
}

// create writes a new pending record; AlreadyExists means a concurrent
// Start took the name, which the caller turns into a retry.
func (b *SecretBackend) create(ctx context.Context, rec release.Record) error {
	sec, err := b.secretObject(rec)
	if err != nil {
		return err
	}
	_, err = b.dyn.Resource(secretsGVR).Namespace(b.namespace).Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return errSeqRace
	}
	if err != nil {
		return fmt.Errorf("writing release v%d: %w", rec.Sequence, err)
	}
	return nil
}

// write replaces an existing record's Secret (Start's takeover
// promotion and Promote both go through it).
func (b *SecretBackend) write(ctx context.Context, rec release.Record) error {
	sec, err := b.secretObject(rec)
	if err != nil {
		return err
	}
	_, err = b.dyn.Resource(secretsGVR).Namespace(b.namespace).Update(ctx, sec, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("writing release v%d: %w", rec.Sequence, err)
	}
	return nil
}

// secretObject renders a record as its Secret: JSON in data.release,
// base64 as the API's wire form requires, flow + sequence labels for
// scoped lists.
func (b *SecretBackend) secretObject(rec release.Record) (*unstructured.Unstructured, error) {
	data, err := release.Encode(rec)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      releaseSecretName(b.flow, rec.Sequence),
			"namespace": b.namespace,
			"labels": map[string]any{
				releaseFlowLabel: b.flow,
				releaseSeqLabel:  strconv.Itoa(rec.Sequence),
			},
		},
		"data": map[string]any{releaseDataKey: base64.StdEncoding.EncodeToString(data)},
	}}, nil
}

// prune deletes records beyond the newest release.KeepCount.
func (b *SecretBackend) prune(ctx context.Context) error {
	records, err := b.List(ctx)
	if err != nil {
		return err
	}
	for i, rec := range records {
		if i < release.KeepCount {
			continue
		}
		err := b.dyn.Resource(secretsGVR).Namespace(b.namespace).
			Delete(ctx, releaseSecretName(b.flow, rec.Sequence), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("pruning release v%d: %w", rec.Sequence, err)
		}
	}
	return nil
}

// decodeSecret lifts a record Secret's data.release back into a Record.
func decodeSecret(sec *unstructured.Unstructured) (release.Record, error) {
	raw, ok, err := unstructured.NestedString(sec.Object, "data", releaseDataKey)
	if err != nil {
		return release.Record{}, fmt.Errorf("reading data.%s: %v", releaseDataKey, err)
	}
	if !ok {
		return release.Record{}, fmt.Errorf("missing data.%s", releaseDataKey)
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return release.Record{}, fmt.Errorf("decoding data.%s: %v", releaseDataKey, err)
	}
	return release.Decode(data)
}
