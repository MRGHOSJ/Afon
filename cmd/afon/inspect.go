package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MRGHOSJ/afon/pkg/probe"
	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var (
	flagNamespace string
	flagFull      bool
	flagRedact    bool
	flagOutput    string
)

var inspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Probe the cluster and print its profile as text or JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runInspect(cmd.Context())
	},
}

func init() {
	inspectCmd.Flags().StringVarP(&flagNamespace, "namespace", "n", "",
		"target namespace (default: kubeconfig context namespace)")
	inspectCmd.Flags().BoolVar(&flagFull, "full", false,
		"include the full API resource list and raw /readyz text in the profile")
	inspectCmd.Flags().BoolVar(&flagRedact, "redact", false,
		"scrub server URL, node names, and IPs before printing")
	inspectCmd.Flags().StringVarP(&flagOutput, "output", "o", "text",
		"output format: text or json")
	rootCmd.AddCommand(inspectCmd)
}

func runInspect(ctx context.Context) error {
	switch flagOutput {
	case "text", "json":
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", flagOutput)
	}
	cfg := clientConfig()

	raw, err := cfg.RawConfig()
	if err != nil {
		return kubeconfigError(err)
	}
	// The effective context: --context wins over the file's current-context,
	// and every message below must name the one actually used.
	contextName := raw.CurrentContext
	if kubeContext != "" {
		contextName = kubeContext
	}
	// Validate it before building the client: clientcmd's own error is
	// untyped, and this way the message can list what the kubeconfig has.
	if err := checkContext(&raw, contextName); err != nil {
		return err
	}

	restCfg, err := cfg.ClientConfig()
	if err != nil {
		return kubeconfigError(err)
	}
	// ~15 probes run in parallel, each fanning out further calls; client-go's
	// default QPS/Burst throttles that.
	restCfg.QPS = 100
	restCfg.Burst = 200
	// ServerVersion and ServerGroupsAndResources ignore the probe context
	// (client-go invokes them with context.Background), so the per-request
	// timeout is what bounds them; without it a blackholed server hangs the
	// whole profile instead of failing.
	restCfg.Timeout = probe.ProbeTimeout

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	ns := flagNamespace
	if ns == "" {
		if ns, _, err = cfg.Namespace(); err != nil || ns == "" {
			ns = "default"
		}
	}
	clusterName := ""
	if ctxEntry := raw.Contexts[contextName]; ctxEntry != nil {
		clusterName = ctxEntry.Cluster
	}

	prof, err := probe.Probe(ctx, probe.Input{
		Client:      cs,
		Dynamic:     dyn,
		RESTConfig:  restCfg,
		KubeContext: contextName,
		ClusterName: clusterName,
		Namespace:   ns,
		Full:        flagFull,
	})
	if err != nil {
		return err
	}
	if flagRedact {
		probe.Redact(&prof)
	}

	if flagOutput == "text" {
		fmt.Print(renderSummary(prof, flagFull))
		return nil
	}
	out, err := json.MarshalIndent(prof, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding profile: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

// kubeconfigError turns clientcmd's raw failures into actionable messages.
// An empty config is the common first-run case (no ~/.kube/config yet) and
// deserves the three ways to fix it; everything else keeps the wrapped cause.
func kubeconfigError(err error) error {
	if clientcmd.IsEmptyConfig(err) {
		return fmt.Errorf("no kubeconfig found set KUBECONFIG, create %s, or pass --kubeconfig <path>",
			clientcmd.NewDefaultClientConfigLoadingRules().GetDefaultFilename())
	}
	return fmt.Errorf("loading kubeconfig: %w", err)
}

// checkContext fails early when the selected context is absent from the
// kubeconfig, listing the ones that are, so a typo says what to type next
// instead of just "does not exist".
func checkContext(raw *clientcmdapi.Config, name string) error {
	if name == "" {
		return nil
	}
	if _, ok := raw.Contexts[name]; ok {
		return nil
	}
	names := make([]string, 0, len(raw.Contexts))
	for n := range raw.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("context %q does not exist the kubeconfig has no contexts", name)
	}
	return fmt.Errorf("context %q does not exist in kubeconfig (available: %s)",
		name, strings.Join(names, ", "))
}
