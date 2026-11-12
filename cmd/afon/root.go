package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfig  string
	kubeContext string
)

var rootCmd = &cobra.Command{
	Use:           "afon",
	Short:         "afon inspects Kubernetes clusters",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return err
	}
	return nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	rootCmd.PersistentFlags().StringVar(&kubeContext, "context", "",
		"kubeconfig context to use; for k8s plan/apply set it here or in the flow's spec.target.context (the kubeconfig's current context is not used implicitly)")
}

// clientConfig honors the --context flag (falling back to the
// kubeconfig's current context) — the form inspect commands use,
// where there is no flow to consult.
func clientConfig() clientcmd.ClientConfig {
	return clientConfigFor(kubeContext)
}

// clientConfigFor loads the kubeconfig with an explicit context
// override. An empty contextName falls back to the kubeconfig's
// current context.
func clientConfigFor(contextName string) clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
}

// resolveContext picks the kubeconfig context for a k8s driver run.
// The --context flag and the flow's target.context must agree when
// both are set — a mismatch is an error, never a guess — and either
// alone is authoritative. With neither the run fails: the
// kubeconfig's current context is deliberately not trusted
// implicitly, so a flow cannot land on production because of what
// happened to be current.
func resolveContext(flagCtx, flowCtx string) (string, error) {
	switch {
	case flagCtx != "" && flowCtx != "" && flagCtx != flowCtx:
		return "", fmt.Errorf(
			"--context %q disagrees with the flow's target.context %q; pass --context %q or set target.context to %q",
			flagCtx, flowCtx, flowCtx, flagCtx)
	case flagCtx != "":
		return flagCtx, nil
	case flowCtx != "":
		return flowCtx, nil
	default:
		return "", fmt.Errorf(
			"no context: pass --context or set spec.target.context in the flow (the kubeconfig's current context is not used implicitly)")
	}
}
