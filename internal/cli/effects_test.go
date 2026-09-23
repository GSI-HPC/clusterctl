// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"testing"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/safety"
)

// TestEveryCommandHasAnEffect keeps the effects table and the command tree in
// step: a new command cannot be added without saying what it does, and an
// entry cannot outlive its command.
func TestEveryCommandHasAnEffect(t *testing.T) {
	cmd, _ := newRoot(context.Background(), app.Streams{})

	seen := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.HasSubCommands() || c == cmd {
			return
		}
		path := commandPath(c)
		seen[path] = true
		if _, ok := effects[path]; !ok {
			t.Errorf("%q has no entry in the effects table", path)
			return
		}
		if got := safety.EffectOf(c.Annotations); got != effects[path] {
			t.Errorf("%q is marked %q, want %q", path, got, effects[path])
		}
	}
	walk(cmd)

	for path := range effects {
		if !seen[path] {
			t.Errorf("the effects table names %q, which is not a command", path)
		}
	}
}

// TestNothingThatWritesIsMarkedRead spot-checks the commands whose names
// sound harmless but are not.
func TestNothingThatWritesIsMarkedRead(t *testing.T) {
	for _, path := range []string{
		"bmc power", "hca config", "slurm account shares", "hostkey refresh",
		"bmc forget", "exec", "copy", "tunnel start", "login", "mcp serve",
	} {
		if effects[path] == safety.EffectRead {
			t.Errorf("%q is marked read-only", path)
		}
	}
}
