package release

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fullRecord exercises every field at once.
func fullRecord() Record {
	return Record{
		APIVersion:       APIVersion,
		Flow:             "web",
		Sequence:         3,
		Status:           StatusSucceeded,
		Trigger:          TriggerManual,
		PreviousSequence: 2,
		StartedAt:        time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
		FinishedAt:       time.Date(2026, 11, 13, 10, 2, 30, 0, time.UTC),
		Manifests: []ManifestStream{
			{Node: "r1", YAML: "apiVersion: v1\nkind: ConfigMap\n"},
			{Node: "r2", YAML: "apiVersion: v1\nkind: Service\n"},
		},
		Inventory: []InventoryObject{
			{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "a", Node: "r1", UID: "uid-1", Hash: "aa"},
			{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "demo", Name: "b", Node: "r2", UID: "uid-2", Hash: "bb"},
		},
		Error: "",
	}
}

func TestRecordJSONRoundTrip(t *testing.T) {
	want := fullRecord()
	b, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Flow != want.Flow || got.Sequence != want.Sequence ||
		got.Status != want.Status || got.Trigger != want.Trigger ||
		got.PreviousSequence != want.PreviousSequence ||
		!got.StartedAt.Equal(want.StartedAt) || !got.FinishedAt.Equal(want.FinishedAt) {
		t.Fatalf("round trip lost header fields:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Manifests) != 2 || got.Manifests[0].Node != "r1" || got.Manifests[1].YAML != want.Manifests[1].YAML {
		t.Fatalf("round trip lost manifests: %+v", got.Manifests)
	}
	if len(got.Inventory) != 2 || got.Inventory[1].UID != "uid-2" || got.Inventory[1].Node != "r2" {
		t.Fatalf("round trip lost inventory: %+v", got.Inventory)
	}
}

func TestEncodeDeterministic(t *testing.T) {
	rec := fullRecord()
	a, err := Encode(rec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	b, err := Encode(rec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("Encode is not deterministic:\n%s\n%s", a, b)
	}
	// The wire form must carry the version so Decode can refuse
	// unknown formats.
	if !strings.Contains(string(a), `"apiVersion":"afon.release/v1"`) {
		t.Fatalf("encoded record missing apiVersion: %s", a)
	}
}

func TestDecodeRejectsUnknownAPIVersion(t *testing.T) {
	rec := fullRecord()
	rec.APIVersion = "afon.release/v99"
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := Decode(b); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Decode of future apiVersion: err = %v, want 'not supported'", err)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Fatal("Decode of garbage: want error, got nil")
	}
}

func TestRecordValidate(t *testing.T) {
	base := func() Record {
		return Record{
			APIVersion: APIVersion,
			Flow:       "web",
			Sequence:   1,
			Status:     StatusPending,
			Trigger:    TriggerManual,
			StartedAt:  time.Now(),
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Record)
		wantSub string
	}{
		{"ok pending", func(r *Record) {}, ""},
		{"ok succeeded", func(r *Record) {
			r.Status = StatusSucceeded
			r.FinishedAt = time.Now()
		}, ""},
		{"ok rolledback", func(r *Record) {
			r.Status = StatusRolledBack
			r.FinishedAt = time.Now()
		}, ""},
		{"missing apiVersion", func(r *Record) { r.APIVersion = "" }, "apiVersion"},
		{"wrong apiVersion", func(r *Record) { r.APIVersion = "afon.release/v0" }, "apiVersion"},
		{"missing flow", func(r *Record) { r.Flow = "" }, "flow is required"},
		{"zero sequence", func(r *Record) { r.Sequence = 0 }, "sequence"},
		{"negative sequence", func(r *Record) { r.Sequence = -1 }, "sequence"},
		{"unknown status", func(r *Record) { r.Status = "deployed" }, "unknown status"},
		{"unknown trigger", func(r *Record) { r.Trigger = "webhook" }, "unknown trigger"},
		{"missing startedAt", func(r *Record) { r.StartedAt = time.Time{} }, "startedAt"},
		{"terminal without finishedAt", func(r *Record) {
			r.Status = StatusSucceeded
		}, "finishedAt"},
		{"failed without finishedAt", func(r *Record) {
			r.Status = StatusFailed
		}, "finishedAt"},
		{"ok rolledBackTo past sequence", func(r *Record) {
			r.Sequence = 3
			r.RolledBackTo = 1
		}, ""},
		{"rolledBackTo future sequence", func(r *Record) {
			r.Sequence = 2
			r.RolledBackTo = 2
		}, "rolledBackTo"},
		{"rolledBackTo negative", func(r *Record) { r.RolledBackTo = -1 }, "rolledBackTo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := base()
			tc.mutate(&rec)
			err := rec.Validate()
			switch {
			case tc.wantSub == "" && err != nil:
				t.Fatalf("Validate: %v, want nil", err)
			case tc.wantSub != "" && err == nil:
				t.Fatal("Validate: want error, got nil")
			case tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub):
				t.Fatalf("Validate: err = %q, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestStatusAndTriggerValid(t *testing.T) {
	for _, s := range []Status{StatusPending, StatusSucceeded, StatusFailed, StatusRolledBack} {
		if !s.Valid() {
			t.Errorf("Status(%q).Valid() = false", s)
		}
	}
	for _, s := range []Status{"", "deployed", "superseded"} {
		if s.Valid() {
			t.Errorf("Status(%q).Valid() = true", s)
		}
	}
	for _, tr := range []Trigger{TriggerManual, TriggerAuto, TriggerRollback} {
		if !tr.Valid() {
			t.Errorf("Trigger(%q).Valid() = false", tr)
		}
	}
	for _, tr := range []Trigger{"", "webhook"} {
		if tr.Valid() {
			t.Errorf("Trigger(%q).Valid() = true", tr)
		}
	}
}

func TestIsStale(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		rec  Record
		want bool
	}{
		{"fresh pending", Record{Status: StatusPending, StartedAt: now.Add(-5 * time.Minute)}, false},
		{"just under the boundary", Record{Status: StatusPending, StartedAt: now.Add(-PendingStaleAfter)}, false},
		{"past the boundary", Record{Status: StatusPending, StartedAt: now.Add(-PendingStaleAfter - time.Second)}, true},
		{"old pending", Record{Status: StatusPending, StartedAt: now.Add(-time.Hour)}, true},
		{"stale-looking but succeeded", Record{Status: StatusSucceeded, StartedAt: now.Add(-time.Hour)}, false},
		{"stale-looking but failed", Record{Status: StatusFailed, StartedAt: now.Add(-time.Hour)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rec.IsStale(now); got != tc.want {
				t.Fatalf("IsStale = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRestorable(t *testing.T) {
	cases := map[Status]bool{
		StatusPending:    false,
		StatusSucceeded:  true,
		StatusFailed:     false,
		StatusRolledBack: true,
	}
	for st, want := range cases {
		rec := Record{Status: st}
		if got := rec.Restorable(); got != want {
			t.Errorf("Record{Status:%q}.Restorable() = %v, want %v", st, got, want)
		}
	}
}

func TestErrConcurrentApplyMessages(t *testing.T) {
	started := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	fresh := &ErrConcurrentApply{Flow: "web", Sequence: 4, StartedAt: started}
	msg := fresh.Error()
	if !strings.Contains(msg, "another apply is in progress") {
		t.Errorf("fresh message missing wait phrasing: %q", msg)
	}
	if !strings.Contains(msg, "v4") || !strings.Contains(msg, `"web"`) {
		t.Errorf("fresh message missing sequence/flow: %q", msg)
	}
	if strings.Contains(msg, "--takeover") {
		t.Errorf("fresh message must not offer --takeover (it only works on stale records): %q", msg)
	}

	stale := &ErrConcurrentApply{Flow: "web", Sequence: 4, StartedAt: started, Stale: true}
	msg = stale.Error()
	if !strings.Contains(msg, "probable crashed run") {
		t.Errorf("stale message missing crash phrasing: %q", msg)
	}
	if !strings.Contains(msg, "--takeover") {
		t.Errorf("stale message missing --takeover: %q", msg)
	}
	if !strings.Contains(msg, started.Format(time.RFC3339)) {
		t.Errorf("stale message missing pending-since time: %q", msg)
	}
}
