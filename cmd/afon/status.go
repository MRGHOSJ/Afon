package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/release"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(newStatusCmd())
}

// newStatusCmd wires `afon status`.
func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Compare live cluster state against the latest release record",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runStatus(ctx)
		},
	}
	cmd.Flags().StringVarP(&flagFlowFile, "file", "f", "flow.yaml", "path to the flow file")
	cmd.Flags().StringVar(&flagDriver, "driver", "fake", "driver to use (fake or k8s)")
	cmd.Flags().StringVarP(&flagOutput, "output", "o", "text",
		"output format: text or json")
	return cmd
}

func runStatus(ctx context.Context) error {
	if flagDriver != "k8s" {
		return errReleaseRecordsNeedK8s
	}
	cf, err := parseFlow(flagFlowFile)
	if err != nil {
		return err
	}
	d, _, _, backend, err := buildDriver("", cf.Target, cf.Name)
	if err != nil {
		return err
	}
	records, err := backend.List(ctx)
	if err != nil {
		return err
	}
	out, err := statusOutput(ctx, d, records, cf.Name, flagOutput)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// statusOutput is runStatus's whole decision tree: no records and
// non-comparable records print an info line; a comparable latest
// release is diffed against live state through the driver's optional
// capabilities.
func statusOutput(ctx context.Context, d driver.Driver, records []release.Record, flow, output string) (string, error) {
	if len(records) == 0 {
		return fmt.Sprintf("no releases recorded for flow %q\n", flow), nil
	}
	// List is newest-first.
	latest := records[0]
	if latest.Status == release.StatusPending {
		return fmt.Sprintf("release v%d is pending (apply in progress or crashed; see \"afon history\")\n",
			latest.Sequence), nil
	}
	if !latest.Restorable() {
		return fmt.Sprintf("release v%d: failed (not restorable; see \"afon history\")\n",
			latest.Sequence), nil
	}

	reader, ok := d.(driver.ObjectReader)
	if !ok {
		return "", errors.New("driver does not report live state")
	}
	lister, ok := d.(driver.ObjectLister)
	if !ok {
		return "", errors.New("driver cannot list flow objects")
	}

	// Read live state per node: Read scopes ownership by node, and a
	// flow-wide owner would read every node's labels as foreign.
	lives := make([]driver.LiveObject, 0, len(latest.Inventory))
	byNode := map[string][]driver.Object{}
	for _, e := range latest.Inventory {
		byNode[e.Node] = append(byNode[e.Node], driver.Object{
			Group: e.Group, Version: e.Version, Kind: e.Kind,
			Namespace: e.Namespace, Name: e.Name,
		})
	}
	for node, objs := range byNode {
		got, err := reader.Read(ctx, driver.Owner{Flow: flow, Node: node}, objs)
		if err != nil {
			return "", fmt.Errorf("reading live state: %w", err)
		}
		lives = append(lives, got...)
	}

	// Extras: flow-owned objects among every kind any record mentions —
	// a kind the current flow dropped still shows up through an older
	// record's inventory.
	owned, err := lister.ListOwned(ctx, driver.Owner{Flow: flow}, recordKinds(records))
	if err != nil {
		return "", fmt.Errorf("listing flow objects: %w", err)
	}

	rep := compareInventory(latest.Inventory, lives, owned)
	rep.Release = statusRelease{
		Sequence:  latest.Sequence,
		Status:    latest.Status,
		Trigger:   latest.Trigger,
		StartedAt: latest.StartedAt,
	}
	return formatStatus(rep, output)
}

// recordKinds dedupes the kinds every record's inventory mentions,
// sorted for deterministic listings.
func recordKinds(records []release.Record) []driver.ObjectKind {
	seen := map[driver.ObjectKind]bool{}
	for _, r := range records {
		for _, e := range r.Inventory {
			seen[driver.ObjectKind{Group: e.Group, Kind: e.Kind}] = true
		}
	}
	kinds := make([]driver.ObjectKind, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if kinds[i].Group != kinds[j].Group {
			return kinds[i].Group < kinds[j].Group
		}
		return kinds[i].Kind < kinds[j].Kind
	})
	return kinds
}

// statusObject is one object in a status report: identity plus, where
// it matters, the recorded and live UID/hash that disagreed.
type statusObject struct {
	Group        string `json:"group,omitempty"`
	Version      string `json:"version"`
	Kind         string `json:"kind"`
	Namespace    string `json:"namespace,omitempty"`
	Name         string `json:"name"`
	Node         string `json:"node,omitempty"`
	UID          string `json:"uid,omitempty"`
	LiveUID      string `json:"liveUid,omitempty"`
	RecordedHash string `json:"recordedHash,omitempty"`
	LiveHash     string `json:"liveHash,omitempty"`
}

// String renders the object for the text report: Kind/namespace/name.
func (o statusObject) String() string {
	id := o.Kind
	if o.Namespace != "" {
		id += "/" + o.Namespace
	}
	return id + "/" + o.Name
}

// statusRelease summarizes the release the comparison ran against.
type statusRelease struct {
	Sequence  int             `json:"sequence"`
	Status    release.Status  `json:"status"`
	Trigger   release.Trigger `json:"trigger"`
	StartedAt time.Time       `json:"startedAt"`
}

// statusReport is the comparison result: every recorded object landed
// in exactly one bucket, and extras are flow-owned objects no
// inventory entry claims. Slices are always non-nil so -o json is
// stable.
type statusReport struct {
	Release   statusRelease  `json:"release"`
	OK        []statusObject `json:"ok"`
	Missing   []statusObject `json:"missing"`
	Changed   []statusObject `json:"changed"`
	Recreated []statusObject `json:"recreated"`
	Extra     []statusObject `json:"extra"`
}

// invKey identifies an object across record and live forms. The
// version is deliberately excluded: the record stores the version the
// API served then, and a preferred-version change must not fake a
// missing object.
func invKey(group, kind, namespace, name string) string {
	return group + "/" + kind + "/" + namespace + "/" + name
}

// compareInventory buckets every recorded object against live state —
// missing (gone), recreated (UID moved: deleted and re-created behind
// afon's back), changed (same UID, content hash moved), ok — and
// reports flow-owned live objects no record claims as extra.
func compareInventory(inv []release.InventoryObject, lives []driver.LiveObject, owned []driver.Object) *statusReport {
	rep := &statusReport{
		OK:        []statusObject{},
		Missing:   []statusObject{},
		Changed:   []statusObject{},
		Recreated: []statusObject{},
		Extra:     []statusObject{},
	}
	liveByKey := make(map[string]driver.LiveObject, len(lives))
	for _, l := range lives {
		liveByKey[invKey(l.Object.Group, l.Object.Kind, l.Object.Namespace, l.Object.Name)] = l
	}
	recorded := make(map[string]bool, len(inv))
	for _, e := range inv {
		key := invKey(e.Group, e.Kind, e.Namespace, e.Name)
		recorded[key] = true
		obj := statusObject{
			Group: e.Group, Version: e.Version, Kind: e.Kind,
			Namespace: e.Namespace, Name: e.Name, Node: e.Node, UID: e.UID,
		}
		live, found := liveByKey[key]
		switch {
		case !found:
			rep.Missing = append(rep.Missing, obj)
		case live.UID != e.UID:
			obj.LiveUID = live.UID
			rep.Recreated = append(rep.Recreated, obj)
		case live.Hash != e.Hash:
			obj.RecordedHash = e.Hash
			obj.LiveHash = live.Hash
			rep.Changed = append(rep.Changed, obj)
		default:
			rep.OK = append(rep.OK, obj)
		}
	}
	for _, o := range owned {
		if !recorded[invKey(o.Group, o.Kind, o.Namespace, o.Name)] {
			rep.Extra = append(rep.Extra, statusObject{
				Group: o.Group, Version: o.Version, Kind: o.Kind,
				Namespace: o.Namespace, Name: o.Name,
			})
		}
	}
	return rep
}

// formatStatus renders a report: the release header plus an ok count,
// then one section per problem bucket (empty buckets omitted).
func formatStatus(rep *statusReport, output string) (string, error) {
	switch output {
	case "json":
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	case "text":
		var b strings.Builder
		fmt.Fprintf(&b, "release v%d: %s (%s, started %s ago)\n",
			rep.Release.Sequence, rep.Release.Status, rep.Release.Trigger,
			ageString(rep.Release.StartedAt))
		fmt.Fprintf(&b, "  ok %d\n", len(rep.OK))
		sections := []struct {
			label string
			objs  []statusObject
		}{
			{"missing", rep.Missing},
			{"changed", rep.Changed},
			{"recreated", rep.Recreated},
			{"extra", rep.Extra},
		}
		for _, s := range sections {
			if len(s.objs) == 0 {
				continue
			}
			fmt.Fprintf(&b, "  %s %d:\n", s.label, len(s.objs))
			for _, o := range s.objs {
				fmt.Fprintf(&b, "    %s\n", o)
			}
		}
		return b.String(), nil
	default:
		return "", fmt.Errorf("unknown output format %q (available: text, json)", output)
	}
}
