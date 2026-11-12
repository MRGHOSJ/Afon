package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"syscall"

	"github.com/MRGHOSJ/afon/pkg/engine"
	"github.com/spf13/cobra"
)

var (
	flagRenderNode  string
	flagRenderAll   bool
	flagRenderForce bool
	flagRenderCheck bool
	flagRenderOut   string
)

func init() {
	rootCmd.AddCommand(newRenderCmd())
}

// newRenderCmd wires `afon render`: the explicit build step that runs
// action nodes and writes materialized outputs to their out: paths
// with a provenance header. plan/apply never render implicitly — a
// missing materialized file is an error naming this command.
func newRenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Run action nodes and write materialized outputs to files",
		Long: `Render runs the flow's action nodes (materialized nodes plus the
action ancestors they need for reference resolution) and writes each
out: file with a provenance header that records the config and body
hashes, so later runs can detect hand-edits and stale renders.

plan and apply never render implicitly: a missing file fails with a
message pointing back here. Status lines go to stderr; rendered YAML
(ephemeral outputs bare, materialized files with their header) goes
to stdout.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runRender(ctx)
		},
	}
	cmd.Flags().StringVarP(&flagFlowFile, "file", "f", "flow.yaml", "path to the flow file")
	cmd.Flags().StringVar(&flagRenderNode, "node", "", "render only this node plus its action ancestors")
	cmd.Flags().BoolVar(&flagRenderAll, "all", false,
		"render every action node (default: materialized nodes only)")
	cmd.Flags().BoolVar(&flagRenderForce, "force", false, "overwrite hand-edited files")
	cmd.Flags().BoolVar(&flagRenderCheck, "check", false,
		"report each file's status (written/unchanged/up-to-date/edited/stale/missing) without writing")
	cmd.Flags().StringVar(&flagRenderOut, "out", "",
		"write the --node output to this path instead of its configured out:")
	return cmd
}

func runRender(ctx context.Context) error {
	if flagRenderNode != "" && flagRenderAll {
		return fmt.Errorf("--node and --all are mutually exclusive")
	}
	if flagRenderOut != "" && flagRenderNode == "" {
		return fmt.Errorf("--out requires --node")
	}
	cf, err := parseFlow(flagFlowFile)
	if err != nil {
		return err
	}
	mode := engine.RenderMaterialized
	if flagRenderAll {
		mode = engine.RenderAll
	}
	res, err := engine.RenderActions(ctx, cf, engine.RenderOptions{
		Force: flagRenderForce,
		Check: flagRenderCheck,
		Only:  flagRenderNode,
		Mode:  mode,
		Out:   flagRenderOut,
	})
	if err != nil {
		return err
	}
	printRenderResult(res)
	return nil
}

// printRenderResult sends status lines to stderr so stdout stays a
// clean YAML stream: file contents (header included) in topological
// order, then the bare outputs of ephemeral action nodes that ran
// without writing anything.
func printRenderResult(res *engine.RenderResult) {
	written := map[string]bool{}
	for _, f := range res.Files {
		written[f.NodeID] = true
		fmt.Fprintf(os.Stderr, "%s: node %s → %s\n", f.Status, f.NodeID, f.Path)
		if f.Diff != "" {
			for _, line := range splitLines(f.Diff) {
				fmt.Fprintln(os.Stderr, "  "+line)
			}
		}
		fmt.Print(f.Content)
	}
	ids := make([]string, 0, len(res.Outputs))
	for id := range res.Outputs {
		if !written[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		outs := res.Outputs[id]
		keys := make([]string, 0, len(outs))
		for k := range outs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if o := outs[k]; o.Known {
				fmt.Fprintln(os.Stdout, o.Data)
			}
		}
		fmt.Fprintf(os.Stderr, "node %s: ephemeral, nothing written\n", id)
	}
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
