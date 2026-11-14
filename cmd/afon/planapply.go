package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MRGHOSJ/afon/pkg/build"
	"github.com/MRGHOSJ/afon/pkg/driver"
	"github.com/MRGHOSJ/afon/pkg/driver/fake"
	"github.com/MRGHOSJ/afon/pkg/driver/k8s"
	"github.com/MRGHOSJ/afon/pkg/engine"
	"github.com/MRGHOSJ/afon/pkg/release"
	"github.com/MRGHOSJ/afon/pkg/schema"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

var (
	flagDriver         string
	flagStateDir       string
	flagParallelism    int
	flagForceConflicts bool
	flagNoPrune        bool
	flagShowRendered   bool
	flagAutoApprove    bool
	flagNoWait         bool
	flagWaitTimeout    time.Duration
	flagTakeover       bool
)

// stdinIsTerminal reports whether stdin is an interactive terminal.
// It is a variable so tests can inject a TTY (or a pipe) without
// touching the real process stdin.
var stdinIsTerminal = func() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

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
	cmd.Flags().BoolVar(&flagShowRendered, "show-rendered", false,
		"print each known action output from the plan to stderr before executing")
	cmd.Flags().BoolVar(&flagAutoApprove, "auto-approve", false,
		"skip the delete-confirmation prompt (no effect when the plan has no deletes)")
	cmd.Flags().BoolVar(&flagNoWait, "no-wait", false,
		"do not wait for created/updated objects to become ready")
	cmd.Flags().DurationVar(&flagWaitTimeout, "wait-timeout", 5*time.Minute,
		"how long to wait for created/updated objects to become ready (0 = wait indefinitely)")
	cmd.Flags().BoolVar(&flagTakeover, "takeover", false,
		"supersede a stale pending release record (probable crashed run); fresh pending records still block")
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
// loads and before any connection is attempted. The returned context
// name (empty for fake) feeds the plan header. The release backend
// (k8s only; nil for fake) shares the driver's client and namespace,
// scoped to flowName — plan prunes inventory through it, apply writes
// records and prunes inventory through it. The returned build runner
// (k8s only; nil for fake) is what apply attaches as the cluster for
// image-build Nodes — one kubeconfig load feeds both clients.
func buildDriver(stateDir string, target schema.Target, flowName string) (driver.Driver, *build.Runner, string, release.Backend, error) {
	switch flagDriver {
	case "fake":
		fmt.Fprintln(os.Stderr, "fake driver: no cluster is touched")
		d, err := fake.NewPersistent(fakeStatePath(stateDir))
		return d, nil, "", nil, err
	case "k8s":
		if target.Namespace == "" {
			return nil, nil, "", nil, fmt.Errorf("the k8s driver needs spec.target.namespace in the flow")
		}
		ctxName, err := resolveContext(kubeContext, target.Context)
		if err != nil {
			return nil, nil, "", nil, err
		}
		restCfg, err := clientConfigFor(ctxName).ClientConfig()
		if err != nil {
			return nil, nil, "", nil, kubeconfigError(err)
		}
		fmt.Fprintf(os.Stderr, "k8s driver: context=%s namespace=%s\n", ctxName, target.Namespace)
		d, err := k8s.NewFromConfig(restCfg, target.Namespace, flagForceConflicts, !flagNoPrune)
		if err != nil {
			return nil, nil, "", nil, err
		}
		cs, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, nil, "", nil, err
		}
		backend, err := d.Backend(flowName)
		if err != nil {
			return nil, nil, "", nil, err
		}
		return d, build.NewRunner(cs), ctxName, backend, nil
	default:
		return nil, nil, "", nil, fmt.Errorf("unknown driver %q (available: fake, k8s)", flagDriver)
	}
}

// clusterValue adapts the driver's runner to ExecutorOptions.Cluster.
// The fake driver's nil runner must arrive as an UNtyped nil: a typed
// nil in the interface counts as non-nil and would route image-build
// into RunCluster (and only then trip the runner-is-nil guard), while
// the untyped nil keeps dispatch on Run's clear "requires the k8s
// driver (apply only)" refusal.
func clusterValue(r *build.Runner) any {
	if r == nil {
		return nil
	}
	return r
}

// planPruneBackend returns the backend plan should prune inventory
// with — nil when pruning is off (--no-prune), so the inventory half
// of pruning agrees with the driver's label-prune half.
func planPruneBackend(backend release.Backend) release.Backend {
	if flagNoPrune {
		return nil
	}
	return backend
}

// printPlanHeader writes the plan's one-line header to stdout before
// the plan body — text mode only, so -o json output stays a single
// parseable document. It names the driver and, for k8s, the exact
// cluster, namespace, and flag state the plan ran with.
func printPlanHeader(driverName, ctxName string, cf *engine.CompiledFlow) {
	if driverName != "k8s" {
		fmt.Printf("driver: %s\n", driverName)
		return
	}
	force := "off"
	if flagForceConflicts {
		force = "on"
	}
	prune := "off"
	if !flagNoPrune {
		prune = "on"
	}
	fmt.Printf("driver: k8s context: %s namespace: %s force: %s prune: %s\n",
		ctxName, cf.Target.Namespace, force, prune)
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
	if err := warnStaleMaterialized(cf); err != nil {
		return err
	}
	d, _, ctxName, backend, err := buildDriver(stateDir, cf.Target, cf.Name)
	if err != nil {
		return err
	}
	p, err := engine.BuildPlanOpts(ctx, cf, store, d, engine.PlanOptions{
		Backend: planPruneBackend(backend),
	})
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
	printPlanHeader(flagDriver, ctxName, cf)
	fmt.Print(p.RenderText())
	return nil
}

func runApply(ctx context.Context) error {
	cf, store, stateDir, err := openFlow()
	if err != nil {
		return err
	}
	if err := warnStaleMaterialized(cf); err != nil {
		return err
	}
	d, runner, ctxName, backend, err := buildDriver(stateDir, cf.Target, cf.Name)
	if err != nil {
		return err
	}

	// Apply re-plans internally against fresh state: no saved plans
	// between plan and apply, so nothing goes stale.
	p, err := engine.BuildPlanOpts(ctx, cf, store, d, engine.PlanOptions{
		Backend: planPruneBackend(backend),
	})
	if err != nil {
		return err
	}
	if flagOutput != "json" {
		printPlanHeader(flagDriver, ctxName, cf)
		fmt.Print(p.RenderText())
		fmt.Println()
	}
	if flagShowRendered {
		printActionOutputs(p)
	}
	if err := confirmDeletes(p); err != nil {
		return err
	}

	res, err := engine.Apply(ctx, cf, store, d, engine.ExecutorOptions{
		Parallelism: flagParallelism,
		Wait:        !flagNoWait,
		WaitTimeout: flagWaitTimeout,
		Backend:     backend,
		Takeover:    flagTakeover,
		Prune:       !flagNoPrune,
		// The only Cluster wiring site: image-build builds in the
		// cluster on a cache miss; rollback flows are synthetic
		// resource-only replays and deliberately get no cluster.
		Cluster: clusterValue(runner),
	})
	// Results print even when Apply returns an error (e.g. a cancelled
	// run): the per-node statuses are the point of the output.
	printed := printRunOutcome(res, err)
	// onFailure: rollback — after the failed apply's record is
	// terminal (Apply's deferred promote has run) and its results are
	// printed, replay the previous restorable release.
	rbErr := maybeAutoRollback(ctx, cf, store, d, backend, res)
	switch {
	case printed != nil && rbErr != nil:
		return fmt.Errorf("%w (auto-rollback failed: %v)", printed, rbErr)
	case rbErr != nil:
		return fmt.Errorf("auto-rollback: %w", rbErr)
	}
	return printed
}

// maybeAutoRollback runs the onFailure: rollback path after apply: a
// failed node declaring it arms a replay of the previous restorable
// release, announced on stderr. The automatic path deliberately skips
// the delete-confirmation prompt (the run already failed — there is no
// interactive plan to confirm) and prints its own results after the
// apply's. Returns nil when no rollback is due.
func maybeAutoRollback(ctx context.Context, cf *engine.CompiledFlow, store engine.Store, d driver.Driver, backend release.Backend, res *engine.RunResult) error {
	target, err := engine.AutoRollbackTarget(ctx, cf, backend, res)
	if err != nil {
		return err
	}
	if target == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "rolling back to release %d\n", target)
	rbRes, rbErr := engine.Rollback(ctx, cf, store, d, target, engine.ExecutorOptions{
		Parallelism: flagParallelism,
		Wait:        !flagNoWait,
		WaitTimeout: flagWaitTimeout,
		Backend:     backend,
		Takeover:    flagTakeover,
		Prune:       !flagNoPrune,
	})
	if rbRes != nil {
		if flagOutput == "json" {
			// stdout stays the apply's single document; the rollback
			// result goes to stderr as its own JSON blob.
			if out, mErr := json.MarshalIndent(rbRes.Snapshot(), "", "  "); mErr == nil {
				fmt.Fprintln(os.Stderr, string(out))
			}
		} else {
			fmt.Print(renderRunResult(rbRes))
		}
		for _, w := range rbRes.Warnings() {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
	}
	return rbErr
}

// printRunOutcome prints a run's per-node results (even when the run
// errored — the statuses are the point) plus run-level warnings on
// stderr so -o json stdout stays a single parseable document, then
// returns the run error untouched. Shared by apply and rollback.
func printRunOutcome(res *engine.RunResult, err error) error {
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
	if res != nil {
		for _, w := range res.Warnings() {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
	}
	return err
}

// warnStaleMaterialized prints engine staleness warnings to stderr: a
// materialized file whose recorded config hash no longer matches the
// flow means the flow was edited after render. A warning, never an
// error — the file is still what plan/apply use as-is.
func warnStaleMaterialized(cf *engine.CompiledFlow) error {
	warns, err := engine.StaleMaterialized(cf)
	if err != nil {
		return err
	}
	for _, w := range warns {
		fmt.Fprintf(os.Stderr,
			"warning: node %q: materialized file %s is stale (flow config changed since render); run \"afon render -f %s\"\n",
			w.NodeID, w.Path, flagFlowFile)
	}
	return nil
}

// confirmDeletes gates apply on an explicit yes when the plan will
// delete objects. The count is the plan's OpDelete diffs — the actual
// objects that will be destroyed — not the summary's entry count: a
// mixed create+delete batch is one "delete" entry in the summary but
// several dead objects, and the prompt must never undercount those.
// Non-interactive stdin (pipes, CI) refuses instead of guessing: the
// operator opts in with --auto-approve. The prompt goes to stderr so
// -o json stdout stays a single clean document.
func confirmDeletes(p *engine.Plan) error {
	n := p.DeleteObjectCount()
	if n == 0 || flagAutoApprove {
		return nil
	}
	if !stdinIsTerminal() {
		return fmt.Errorf("plan contains %d delete(s) and stdin is not interactive; pass --auto-approve to apply without confirmation", n)
	}
	fmt.Fprintf(os.Stderr, "plan deletes %d object(s); apply these changes? [y/N] ", n)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return fmt.Errorf("apply aborted")
}

// printActionOutputs dumps the plan's known action outputs to stderr —
// the exact bytes downstream nodes will see — before apply executes.
// stderr keeps `apply -o json` stdout parseable.
func printActionOutputs(p *engine.Plan) {
	ids := make([]string, 0, len(p.ActionOutputs))
	for id := range p.ActionOutputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		outs := p.ActionOutputs[id]
		keys := make([]string, 0, len(outs))
		for k := range outs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o := outs[k]
			fmt.Fprintf(os.Stderr, "--- node %s output %s ---\n", id, k)
			if o.Known {
				fmt.Fprintln(os.Stderr, o.Data)
			} else {
				fmt.Fprintln(os.Stderr, "(unknown)")
			}
		}
	}
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
