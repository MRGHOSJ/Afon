package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MRGHOSJ/afon/pkg/release"
	"github.com/spf13/cobra"
)

// errReleaseRecordsNeedK8s is the gate for every record-reading
// command (history, status, rollback): with the default fake driver
// there are no records to read, and silently showing an empty history
// would look like "nothing ever applied". status and rollback reuse
// it verbatim.
var errReleaseRecordsNeedK8s = errors.New(
	"release records require the k8s driver (pass --driver k8s --context ...)")

func init() {
	rootCmd.AddCommand(newHistoryCmd())
}

// newHistoryCmd wires `afon history`.
func newHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List this flow's release records, newest first",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runHistory(ctx)
		},
	}
	cmd.Flags().StringVarP(&flagFlowFile, "file", "f", "flow.yaml", "path to the flow file")
	cmd.Flags().StringVar(&flagDriver, "driver", "fake", "driver to use (fake or k8s)")
	cmd.Flags().StringVarP(&flagOutput, "output", "o", "text",
		"output format: text or json")
	return cmd
}

func runHistory(ctx context.Context) error {
	// Driver gate first: without k8s there are no records at all, and
	// that message — not a flow-file error — is what the operator
	// needs to hear (decision: default fake driver refuses).
	if flagDriver != "k8s" {
		return errReleaseRecordsNeedK8s
	}
	cf, err := parseFlow(flagFlowFile)
	if err != nil {
		return err
	}
	// stateDir is irrelevant here: only the k8s path runs, and it
	// never touches the cache.
	_, _, _, backend, err := buildDriver("", cf.Target, cf.Name)
	if err != nil {
		return err
	}
	records, err := backend.List(ctx)
	if err != nil {
		return err
	}
	out, err := formatHistory(records, cf.Name, flagOutput)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// formatHistory renders records as the history command's output: the
// SEQ STATUS TRIGGER AGE OBJECTS table (text) or the record list
// (json). An empty flow says so instead of printing a bare header.
func formatHistory(records []release.Record, flow, output string) (string, error) {
	switch output {
	case "json":
		if records == nil {
			records = []release.Record{}
		}
		b, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	case "text":
		return renderHistoryTable(records, flow), nil
	default:
		return "", fmt.Errorf("unknown output format %q (available: text, json)", output)
	}
}

// renderHistoryTable formats records as the history table: SEQ STATUS
// TRIGGER AGE OBJECTS, newest first. No IMAGE column — records carry
// inventory, not images — and AGE counts from StartedAt so a pending
// row shows how long it has been pending.
func renderHistoryTable(records []release.Record, flow string) string {
	if len(records) == 0 {
		return fmt.Sprintf("no releases recorded for flow %q\n", flow)
	}
	rows := make([][]string, 0, len(records)+1)
	rows = append(rows, []string{"SEQ", "STATUS", "TRIGGER", "AGE", "OBJECTS"})
	for _, r := range records {
		rows = append(rows, []string{
			strconv.Itoa(r.Sequence),
			string(r.Status),
			string(r.Trigger),
			ageString(r.StartedAt),
			strconv.Itoa(len(r.Inventory)),
		})
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(cell)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ageString renders a record's age compactly from now: 45s, 12m, 3h,
// 10d. A clock skewed into the future clamps to 0s rather than
// printing a negative.
func ageString(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
