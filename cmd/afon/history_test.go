package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/release"
)

// TestHistoryRequiresK8sDriver: with the default fake driver (or any
// non-k8s driver) history refuses with the locked message instead of
// showing an empty table that would look like "nothing ever applied".
func TestHistoryRequiresK8sDriver(t *testing.T) {
	for _, driverFlag := range []string{"fake", "", "bogus"} {
		t.Run("driver_"+driverFlag, func(t *testing.T) {
			old := flagDriver
			flagDriver = driverFlag
			t.Cleanup(func() { flagDriver = old })

			err := runHistory(context.Background())
			if err == nil {
				t.Fatal("runHistory: want error, got nil")
			}
			want := "release records require the k8s driver (pass --driver k8s --context ...)"
			if err.Error() != want {
				t.Fatalf("err = %q, want %q", err, want)
			}
		})
	}
}

func TestFormatHistoryTable(t *testing.T) {
	now := time.Now()
	records := []release.Record{
		{
			APIVersion: release.APIVersion, Flow: "web", Sequence: 4,
			Status: release.StatusRolledBack, Trigger: release.TriggerRollback,
			StartedAt: now.Add(-90 * time.Second),
			Inventory: make([]release.InventoryObject, 3),
		},
		{
			APIVersion: release.APIVersion, Flow: "web", Sequence: 3,
			Status: release.StatusFailed, Trigger: release.TriggerManual,
			StartedAt: now.Add(-time.Hour),
		},
		{
			APIVersion: release.APIVersion, Flow: "web", Sequence: 2,
			Status: release.StatusSucceeded, Trigger: release.TriggerManual,
			StartedAt: now.Add(-2 * time.Hour),
			Inventory: make([]release.InventoryObject, 5),
		},
	}
	out, err := formatHistory(records, "web", "text")
	if err != nil {
		t.Fatalf("formatHistory: %v", err)
	}
	want := "" +
		"SEQ  STATUS      TRIGGER   AGE  OBJECTS\n" +
		"4    rolledback  rollback  1m   3\n" +
		"3    failed      manual    1h   0\n" +
		"2    succeeded   manual    2h   5\n"
	if out != want {
		t.Fatalf("table:\n%s\nwant:\n%s", out, want)
	}
}

func TestFormatHistoryEmpty(t *testing.T) {
	out, err := formatHistory(nil, "web", "text")
	if err != nil {
		t.Fatalf("formatHistory: %v", err)
	}
	if out != "no releases recorded for flow \"web\"\n" {
		t.Fatalf("empty output = %q", out)
	}
	// nil and [] must both render as an empty JSON array, never null.
	for _, recs := range [][]release.Record{nil, {}} {
		out, err := formatHistory(recs, "web", "json")
		if err != nil {
			t.Fatalf("formatHistory json: %v", err)
		}
		if strings.TrimSpace(out) != "[]" {
			t.Fatalf("json empty = %q, want []", out)
		}
	}
}

func TestFormatHistoryJSON(t *testing.T) {
	records := []release.Record{{
		APIVersion: release.APIVersion, Flow: "web", Sequence: 2,
		Status: release.StatusSucceeded, Trigger: release.TriggerManual,
		StartedAt: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
		Inventory: []release.InventoryObject{{
			Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "a",
			Node: "r1", UID: "uid-a", Hash: "h1",
		}},
	}}
	out, err := formatHistory(records, "web", "json")
	if err != nil {
		t.Fatalf("formatHistory: %v", err)
	}
	var got []release.Record
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Sequence != 2 || got[0].Status != release.StatusSucceeded {
		t.Fatalf("decoded = %+v", got)
	}
	if len(got[0].Inventory) != 1 || got[0].Inventory[0].UID != "uid-a" {
		t.Fatalf("inventory lost in JSON: %+v", got[0].Inventory)
	}
}

func TestFormatHistoryUnknownOutput(t *testing.T) {
	if _, err := formatHistory(nil, "web", "yaml"); err == nil ||
		!strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("err = %v, want unknown output format", err)
	}
}

func TestAgeString(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{2 * time.Minute, "2m"},
		{90 * time.Minute, "1h"},
		{25 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}
	for _, tc := range cases {
		if got := ageString(now.Add(-tc.ago)); got != tc.want {
			t.Errorf("ageString(-%v) = %q, want %q", tc.ago, got, tc.want)
		}
	}
	// Clock skew: a timestamp in the future clamps instead of going
	// negative.
	if got := ageString(now.Add(time.Minute)); got != "0s" {
		t.Errorf("future timestamp = %q, want 0s", got)
	}
}
