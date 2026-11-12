package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/driver/fake"
	"github.com/MRGHOSJ/afon/pkg/driver/k8s"
	"github.com/MRGHOSJ/afon/pkg/engine"
	"github.com/MRGHOSJ/afon/pkg/schema"
	"github.com/spf13/cobra"
)

var (
	flagDriver         string
	flagStateDir       string
	flagParallelism    int
	flagForceConflicts bool
	flagNoPrune        bool
)

// notifyContext wraps context cancellation in a signal handler (SIGINT /
// SIGTERM → cancel). It is a variable so tests can inject a context they
// cancel themselves, exercising the same cancellation path as Ctrl-C
// without sending a real signal to the process.
var notifyContext = signal.NotifyContext

func init() {
	rootCmd.AddCommand(newPlanCmd(), newApplyCmd())
}

// newPlanCmd wires `afon plan`.
func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what an apply would do, without executing anything",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runPlan(ctx)
		},
	}
	addFlowFlags(cmd)
	return cmd
}

// newApplyCmd wires `afon apply`.
func newApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Execute a flow: re-plan, then run against the driver",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runApply(ctx)
		},
	}
	addFlowFlags(cmd)
	return cmd
}

func addFlowFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&flagFlowFile, "file", "f", "flow.yaml", "path to the flow file")
	cmd.Flags().StringVar(&flagDriver, "driver", "fake", "driver to use (fake or k8s)")
	cmd.Flags().StringVar(&flagStateDir, "state-dir", "", "state directory (default: directory of the flow file)")
	cmd.Flags().IntVar(&flagParallelism, "parallelism", 4, "maximum concurrent nodes")
	cmd.Flags().BoolVar(&flagForceConflicts, "force-conflicts", false,
		"take field ownership on conflicts instead of failing (k8s driver; affects plan dry-runs too)")
	cmd.Flags().BoolVar(&flagNoPrune, "no-prune", false,
		"do not delete objects removed from the flow (k8s driver)")
	cmd.Flags().StringVarP(&flagOutput, "output", "o", "text",
		"output format: text or json")
}

// buildDriver selects the driver from the flag — never from the flow file.
// The fake driver banners loudly (on stderr, so -o json stays parseable)
// so nobody mistakes a fake apply for a real one. The k8s driver needs
// the flow's target namespace (it manages exactly one) and a context
// that --context and target.context agree on; it then banners which
// cluster and namespace it is about to touch, after the kubeconfig
// loads and before any connection is attempted.
func buildDriver(stateDir string, target schema.Target) (driver.Driver, error) {
	switch flagDriver {
	case "fake":
		fmt.Fprintln(os.Stderr, "fake driver: no cluster is touched")
		return fake.NewPersistent(fakeStatePath(stateDir))
	case "k8s":
		if target.Namespace == "" {
			return nil, fmt.Errorf("the k8s driver needs spec.target.namespace in the flow")
		}
		ctxName, err := resolveContext(kubeContext, target.Context)
		if err != nil {
			return nil, err
		}
		restCfg, err := clientConfigFor(ctxName).ClientConfig()
		if err != nil {
			return nil, kubeconfigError(err)
		}
		fmt.Fprintf(os.Stderr, "k8s driver: context=%s namespace=%s\n", ctxName, target.Namespace)
		return k8s.NewFromConfig(restCfg, target.Namespace, flagForceConflicts, !flagNoPrune)
	default:
		return nil, fmt.Errorf("unknown driver %q (available: fake, k8s)", flagDriver)
	}
}

func openFlow() (*engine.CompiledFlow, engine.Store, string, error) {
	cf, err := parseFlow(flagFlowFile)
	if err != nil {
		return nil, nil, "", err
	}
	stateDir, err := stateDirFor(flagFlowFile, flagStateDir)
	if err != nil {
		return nil, nil, "", err
	}
	store, err := openStore(stateDir)
	if err != nil {
		return nil, nil, "", err
	}
	return cf, store, stateDir, nil
}

func runPlan(ctx context.Context) error {
	cf, store, stateDir, err := openFlow()
	if err != nil {
		return err
	}
	d, err := buildDriver(stateDir, cf.Target)
	if err != nil {
		return err
	}
	p, err := engine.BuildPlan(ctx, cf, store, d)
	if err != nil {
		return err
	}
	if flagOutput == "json" {
		out, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	fmt.Print(p.RenderText())
	return nil
}

func runApply(ctx context.Context) error {
	cf, store, stateDir, err := openFlow()
	if err != nil {
		return err
	}
	d, err := buildDriver(stateDir, cf.Target)
	if err != nil {
		return err
	}

	// Apply re-plans internally against fresh state: no saved plans
	// between plan and apply, so nothing goes stale.
	p, err := engine.BuildPlan(ctx, cf, store, d)
	if err != nil {
		return err
	}
	if flagOutput != "json" {
		fmt.Print(p.RenderText())
		fmt.Println()
	}

	res, err := engine.Apply(ctx, cf, store, d, engine.ExecutorOptions{
		Parallelism: flagParallelism,
	})
	// Results print even when Apply returns an error (e.g. a cancelled
	// run): the per-node statuses are the point of the output.
	if flagOutput == "json" {
		if res != nil {
			out, mErr := json.MarshalIndent(res.Snapshot(), "", "  ")
			if mErr == nil {
				fmt.Println(string(out))
			}
		}
	} else if res != nil {
		fmt.Print(renderRunResult(res))
	}
	return err
}

// renderRunResult formats per-node apply outcomes. The status column is
// padded to the longest status so every suffix starts at the same column:
// find the status, skip the spaces, and you land on the suffix. Suffix
// priority: error, then cached, then detail — a single space separates
// the status field from a non-space-leading suffix.
func renderRunResult(res *engine.RunResult) string {
	snap := res.Snapshot()
	ids := sortedKeys(snap)
	maxW := 0
	for _, id := range ids {
		if w := len(string(snap[id].Status)); w > maxW {
			maxW = w
		}
	}
	out := "apply result:\n"
	for _, id := range ids {
		r := snap[id]
		line := fmt.Sprintf("  %-16s %-*s", id, maxW, r.Status)
		var suffix string
		switch {
		case r.ErrText != "":
			suffix = ": " + r.ErrText
		case r.Cached:
			suffix = "(cached)"
		case r.Detail != "":
			suffix = "(" + r.Detail + ")"
		}
		if suffix != "" {
			line += " " + suffix
		}
		out += strings.TrimRight(line, " ") + "\n"
	}
	return out
}

func sortedKeys(m map[string]engine.NodeResult) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
