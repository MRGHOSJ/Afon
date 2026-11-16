package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/MRGHOSJ/afon/pkg/engine"
	"github.com/spf13/cobra"
)

// flagRollbackTo is `afon rollback --to`; 0 means "resolve the default
// (the release before the latest)".
var flagRollbackTo int

// rollbackLimits is design §3's honesty notice: a rollback restores
// recorded manifests, nothing else. Printed with every rollback plan,
// before the confirmation prompt.
const rollbackLimits = "rollback limits: recorded manifests only — does not undo database migrations, volume contents, CRD schema changes, or other effects outside the cluster"

func init() {
	rootCmd.AddCommand(newRollbackCmd())
}

// newRollbackCmd wires `afon rollback`.
func newRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "re-apply an earlier release's recorded manifests",
		Long: `rollback replays the manifests of a previous release exactly as they
were applied, prunes what that release does not own, and records the
result as a new release (append-only). It does not undo database
migrations, volume contents, CRD schema changes, or other effects
outside the cluster.

With no --to, the release before the latest is replayed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runRollback(ctx)
		},
	}
	addFlowFlags(cmd)
	cmd.Flags().IntVar(&flagRollbackTo, "to", 0,
		"release sequence to roll back to (default: the release before the latest; see afon history)")
	cmd.Flags().BoolVar(&flagAutoApprove, "auto-approve", false,
		"skip the delete-confirmation prompt (no effect when the plan has no deletes)")
	cmd.Flags().BoolVar(&flagNoWait, "no-wait", false,
		"do not wait for restored objects to become ready")
	cmd.Flags().DurationVar(&flagWaitTimeout, "wait-timeout", 5*time.Minute,
		"how long to wait for restored objects to become ready (0 = wait indefinitely)")
	cmd.Flags().BoolVar(&flagTakeover, "takeover", false,
		"supersede a stale pending release record (probable crashed run); fresh pending records still block")
	return cmd
}

func runRollback(ctx context.Context) error {
	if flagDriver != "k8s" {
		return errReleaseRecordsNeedK8s
	}
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
	if err := ensureTargetNamespace(ctx, d, cf.Target.Namespace); err != nil {
		return err
	}

	to := flagRollbackTo
	switch {
	case to < 0:
		return fmt.Errorf("--to must be a positive release sequence, got %d", to)
	case to == 0:
		to, err = engine.DefaultRollbackTarget(ctx, backend, cf.Name)
		if err != nil {
			return err
		}
	}

	// Plan first (dry run), with the limits notice, then the same
	// delete confirmation apply gets — the auto-rollback path (6f)
	// calls engine.Rollback directly and deliberately skips this.
	p, err := engine.RollbackPlan(ctx, cf, backend, d, to)
	if err != nil {
		return err
	}
	if flagOutput == "json" {
		out, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		fmt.Fprintln(os.Stderr, rollbackLimits)
	} else {
		printPlanHeader(flagDriver, ctxName, cf)
		fmt.Print(p.RenderText())
		fmt.Println(rollbackLimits)
	}
	if err := confirmDeletes(p); err != nil {
		return err
	}

	res, err := engine.Rollback(ctx, cf, store, d, to, engine.ExecutorOptions{
		Parallelism: flagParallelism,
		Wait:        !flagNoWait,
		WaitTimeout: flagWaitTimeout,
		Backend:     backend,
		Takeover:    flagTakeover,
		Prune:       !flagNoPrune,
	})
	return printRunOutcome(res, err)
}
