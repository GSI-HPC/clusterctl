// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"io"
	"runtime"
	"testing"
	"weak"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/GSI-HPC/clusterctl/internal/app"
)

// agentTree builds a tree as the MCP server does for each command it runs.
func agentTree() *cobra.Command {
	return CommandTree(nil)(context.Background(), app.Streams{Out: io.Discard, Err: io.Discard})
}

// completed lists the flags of a tree that have a completion function.
func completed(tree *cobra.Command) []string {
	var names []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		// The persistent flags join a command's own only when it runs.
		visit := func(f *pflag.Flag) {
			if _, ok := c.GetFlagCompletionFunc(f.Name); ok {
				names = append(names, c.CommandPath()+" --"+f.Name)
			}
		}
		c.Flags().VisitAll(visit)
		c.PersistentFlags().VisitAll(visit)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(tree)
	return names
}

// TestAnAgentTreeRegistersNoCompletion checks that the trees the MCP server
// builds leave nothing in cobra's map of completion functions, which is
// global, keyed by the flag and never emptied. It grew by every flag of two
// trees for each read_command, each function holding the root and the
// configuration the command had read: some 50 MB a call at 10,000 nodes.
func TestAnAgentTreeRegistersNoCompletion(t *testing.T) {
	shell, _ := newRoot(context.Background(), app.Streams{Out: io.Discard, Err: io.Discard})
	if len(completed(shell)) == 0 {
		t.Fatal("the shell's tree completes no flag, so this test would see nothing")
	}
	if got := completed(agentTree()); len(got) > 0 {
		t.Errorf("an agent's tree registers completions for %v", got)
	}
}

// TestAnAgentTreeIsFreedOnceDropped checks that nothing outlives a tree built
// for an agent: once the server has dropped it, its flags are collected.
func TestAnAgentTreeIsFreedOnceDropped(t *testing.T) {
	flags := dropTree()
	runtime.GC()
	for name, p := range flags {
		if p.Value() != nil {
			t.Errorf("the flag %s of a dropped tree is still reachable", name)
		}
	}
}

// dropTree builds an agent's tree and returns weak pointers to flags that
// have a completion function in the shell's tree, holding nothing else of it.
func dropTree() map[string]weak.Pointer[pflag.Flag] {
	tree := agentTree()
	list, _, err := tree.Find([]string{"slurm", "node", "list"})
	if err != nil {
		panic(err)
	}
	return map[string]weak.Pointer[pflag.Flag]{
		"--nodes":  weak.Make(tree.PersistentFlags().Lookup("nodes")),
		"--output": weak.Make(tree.PersistentFlags().Lookup("output")),
		"--state":  weak.Make(list.Flags().Lookup("state")),
	}
}
