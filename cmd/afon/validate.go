package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var flagFlowFile string

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Parse and compile a flow file without touching a driver",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runValidate()
	},
}

func init() {
	validateCmd.Flags().StringVarP(&flagFlowFile, "file", "f", "flow.yaml", "path to the flow file")
	rootCmd.AddCommand(validateCmd)
}

// runValidate is schema + Compile only (engine rule 5): types, declared
// outputs, and cycles surface here, and no driver is ever constructed.
func runValidate() error {
	cf, err := parseFlow(flagFlowFile)
	if err != nil {
		return err
	}
	fmt.Printf("flow %q is valid: %d nodes, %d levels\n",
		cf.Name, len(cf.Nodes), maxLevel(cf.Levels))
	return nil
}

func maxLevel(levels map[string]int) int {
	max := 0
	for _, l := range levels {
		if l > max {
			max = l
		}
	}
	return max + 1
}
