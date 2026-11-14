// Package release records one release per apply: the record model, the
// Backend interface the engine writes through, and the pure staleness
// rules that guard concurrent applies. It is stdlib-only — the
// Kubernetes implementation lives in pkg/driver/k8s, test doubles live
// with their tests.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// APIVersion identifies the record wire format. A record whose
// apiVersion differs is rejected by every backend: silently reading a
// future format is worse than failing loudly.
const APIVersion = "afon.release/v1"

// PendingStaleAfter bounds how long a pending record may sit before a
// new apply may treat it as a crashed run. The window is deliberately
// generous: it must exceed any plausible apply (waits included), and a
// fresh pending record always blocks — only --takeover crosses it.
const PendingStaleAfter = 10 * time.Minute

// KeepCount is how many records the backend keeps per flow. The newest
// KeepCount are retained; older ones are pruned after each Promote.
const KeepCount = 10

// Status is a record's lifecycle state.
type Status string

const (
	// StatusPending is written before scheduling: the apply is (or was)
	// in flight. It is the concurrency guard — at most one per flow.
	StatusPending Status = "pending"
	// StatusSucceeded means every node finished without failure.
	StatusSucceeded Status = "succeeded"
	// StatusFailed means the run failed or was cancelled; partial state
	// may exist and is inventoried (inventory claims live objects so a
	// later rollback can prune them; manifests stay success-only).
	StatusFailed Status = "failed"
	// StatusRolledBack marks a rollback record whose apply completed.
	StatusRolledBack Status = "rolledback"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusSucceeded, StatusFailed, StatusRolledBack:
		return true
	}
	return false
}

// Trigger says who initiated the recorded operation.
type Trigger string

const (
	// TriggerManual is an explicit afon apply.
	TriggerManual Trigger = "manual"
	// TriggerAuto is reserved for flows initiated by automation
	// (a serve/webhook path); v1 never emits it.
	TriggerAuto Trigger = "auto"
	// TriggerRollback marks records written by rollback, whether the
	// operator asked for it or onFailure: rollback fired it.
	TriggerRollback Trigger = "rollback"
)

// Valid reports whether t is a known trigger.
func (t Trigger) Valid() bool {
	switch t {
	case TriggerManual, TriggerAuto, TriggerRollback:
		return true
	}
	return false
}

// InventoryObject is one live object in a record's inventory: identity
// (group/version/kind/namespace/name), the node that owns it (rollback
// re-applies each object under its recorded owner), and the UID + content
// hash captured at record time for status comparison.
type InventoryObject struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Node is the resource node whose desired set included this object.
	Node string `json:"node"`
	UID  string `json:"uid"`
	// Hash is the hex sha256 of the object's canonical live form
	// (volatile keys stripped) as of record time.
	Hash string `json:"hash"`
}

// ManifestStream is one resource node's YAML stream exactly as it was
// applied. Rollback parses these instead of re-rendering the flow: the
// recorded manifests are the ground truth of what went out.
type ManifestStream struct {
	Node string `json:"node"`
	YAML string `json:"yaml"`
}

// Record is one release: a single apply (or rollback) of one flow,
// from reserved sequence to terminal state.
type Record struct {
	APIVersion       string  `json:"apiVersion"`
	Flow             string  `json:"flow"`
	Sequence         int     `json:"sequence"`
	Status           Status  `json:"status"`
	Trigger          Trigger `json:"trigger"`
	PreviousSequence int     `json:"previousSequence,omitempty"`
	// RolledBackTo is the release sequence a rollback restored from,
	// set only on rollback records (design §3: a rollback writes a new
	// revision naming what it replayed — append-only).
	RolledBackTo int               `json:"rolledBackTo,omitempty"`
	StartedAt    time.Time         `json:"startedAt"`
	FinishedAt   time.Time         `json:"finishedAt"`
	Manifests    []ManifestStream  `json:"manifests,omitempty"`
	Inventory    []InventoryObject `json:"inventory,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// Validate checks the invariants every backend enforces before a
// write. A pending record has no FinishedAt yet; every other status
// must have one (the backend fills it at Promote).
func (r Record) Validate() error {
	switch {
	case r.APIVersion != APIVersion:
		return fmt.Errorf("release record: apiVersion %q not supported (want %q)", r.APIVersion, APIVersion)
	case r.Flow == "":
		return fmt.Errorf("release record: flow is required")
	case r.Sequence < 1:
		return fmt.Errorf("release record: sequence must be >= 1, got %d", r.Sequence)
	case !r.Status.Valid():
		return fmt.Errorf("release record: unknown status %q", r.Status)
	case !r.Trigger.Valid():
		return fmt.Errorf("release record: unknown trigger %q", r.Trigger)
	case r.StartedAt.IsZero():
		return fmt.Errorf("release record: startedAt is required")
	case r.Status != StatusPending && r.FinishedAt.IsZero():
		return fmt.Errorf("release record: finishedAt is required for status %q", r.Status)
	case r.RolledBackTo != 0 && (r.RolledBackTo < 1 || r.RolledBackTo >= r.Sequence):
		return fmt.Errorf("release record: rolledBackTo must be a past sequence (1..%d), got %d",
			r.Sequence-1, r.RolledBackTo)
	}
	return nil
}

// IsStale reports whether a pending record has outlived
// PendingStaleAfter — long enough to treat as a crashed run rather
// than a concurrent one. Only pending records can be stale.
func (r Record) IsStale(now time.Time) bool {
	return r.Status == StatusPending && now.Sub(r.StartedAt) > PendingStaleAfter
}

// Restorable reports whether this record's content represents a
// successfully applied state a rollback can target. Failed records are
// partial and never restorable; pending records are not final.
func (r Record) Restorable() bool {
	return r.Status == StatusSucceeded || r.Status == StatusRolledBack
}

// ErrConcurrentApply reports another apply's pending record. Stale
// tells the two refusal messages apart: a fresh pending record means
// "wait", a stale one means "probable crashed run; --takeover".
type ErrConcurrentApply struct {
	Flow      string
	Sequence  int
	StartedAt time.Time
	Stale     bool
}

func (e *ErrConcurrentApply) Error() string {
	when := e.StartedAt.Format(time.RFC3339)
	if e.Stale {
		return fmt.Sprintf("release v%d for flow %q is pending since %s (probable crashed run); pass --takeover to override",
			e.Sequence, e.Flow, when)
	}
	return fmt.Sprintf("another apply is in progress: release v%d for flow %q pending since %s",
		e.Sequence, e.Flow, when)
}

// Backend persists release records for one flow. Every call takes a
// context; implementations must be safe for concurrent use.
type Backend interface {
	// Start reserves the next sequence and writes the pending record
	// before the apply schedules any node. It refuses with
	// *ErrConcurrentApply when the flow already has a pending record
	// that is fresh, or one that is stale while takeover is false. With
	// takeover, a stale pending record is promoted to failed first. The
	// returned record carries the assigned Sequence, PreviousSequence,
	// and StartedAt with Status still pending.
	Start(ctx context.Context, rec Record, takeover bool) (Record, error)
	// Promote finalizes a record Start returned: status, finishedAt
	// (filled when zero), manifests, inventory, and error. It also
	// prunes records beyond KeepCount. The record is durably written
	// even when pruning then fails.
	Promote(ctx context.Context, rec Record) error
	// Latest returns the highest-sequence record, or ok=false when the
	// flow has none.
	Latest(ctx context.Context) (Record, bool, error)
	// List returns every record for the flow, newest first.
	List(ctx context.Context) ([]Record, error)
	// Get returns one record by sequence, or ok=false when absent.
	Get(ctx context.Context, seq int) (Record, bool, error)
}

// Encode serializes a record in the canonical wire form backends
// store. Deterministic for a given record: JSON object keys are
// sorted, so tests can compare bytes.
func Encode(rec Record) ([]byte, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encoding release v%d: %w", rec.Sequence, err)
	}
	return b, nil
}

// Decode parses a stored record and checks its wire format.
func Decode(data []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("decoding release record: %w", err)
	}
	if rec.APIVersion != APIVersion {
		return rec, fmt.Errorf("release record: apiVersion %q not supported (want %q)", rec.APIVersion, APIVersion)
	}
	return rec, nil
}
