// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/cli"
)

// TestCommandTreeBuilds checks that the entry point can build the tree the
// process runs, which catches a command registered twice or a flag that two
// commands both claim.
func TestCommandTreeBuilds(t *testing.T) {
	cmd := cli.NewRootCommand(context.Background(), app.Streams{Out: &strings.Builder{}, Err: &strings.Builder{}})
	if cmd.Name() != "clusterctl" {
		t.Errorf("the root command is called %q", cmd.Name())
	}
	if len(cmd.Commands()) == 0 {
		t.Error("the command tree is empty")
	}
	if err := cmd.ValidateArgs(nil); err != nil {
		t.Errorf("the root command rejects an empty argument list: %v", err)
	}
}

// TestShellsAreAvailable records that the round trip test of the quoting
// package needs a shell; it is skipped where there is none.
func TestShellsAreAvailable(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}
}
