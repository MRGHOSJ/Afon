package main

import (
	"fmt"
	"path/filepath"

	"github.com/MRGHOSJ/afon/pkg/engine"
	"github.com/MRGHOSJ/afon/pkg/schema"
)

// stateDirFor resolves the state directory: --state-dir wins, otherwise
// the directory containing the flow file. Never the process cwd — two runs
// from different folders must agree on cache and fake state.
func stateDirFor(flowPath, flagDir string) (string, error) {
	if flagDir != "" {
		abs, err := filepath.Abs(flagDir)
		if err != nil {
			return "", fmt.Errorf("resolving --state-dir: %w", err)
		}
		return abs, nil
	}
	abs, err := filepath.Abs(flowPath)
	if err != nil {
		return "", fmt.Errorf("resolving flow path: %w", err)
	}
	return filepath.Dir(abs), nil
}

// newRegistry builds the node-type registry explicitly (engine rule 5):
// no init() magic, so tests can construct their own.
func newRegistry() (*engine.Registry, error) {
	reg := engine.NewRegistry()
	for _, nt := range []engine.NodeType{
		engine.ImageRef{},
		engine.FakeObject{},
	} {
		if err := reg.Register(nt); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// parseFlow parses and compiles a flow file: schema + Compile, no driver
// (engine rule 5 — this is what validate runs).
func parseFlow(path string) (*engine.CompiledFlow, error) {
	flow, err := schema.Parse(path)
	if err != nil {
		return nil, err
	}
	reg, err := newRegistry()
	if err != nil {
		return nil, err
	}
	return engine.Compile(flow, reg)
}

// openStore returns the disk-backed cache under stateDir/.afon/cache.
func openStore(stateDir string) (engine.Store, error) {
	return engine.NewDiskStore(filepath.Join(stateDir, ".afon", "cache"))
}

// fakeStatePath is where the CLI's fake driver persists live objects.
func fakeStatePath(stateDir string) string {
	return filepath.Join(stateDir, ".afon", "fake-state.json")
}
