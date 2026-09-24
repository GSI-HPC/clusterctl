// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// exitCodeOf runs a command line the way the process does, through report,
// and returns the exit code it ends with. run returns the error of Execute
// and skips report, so it cannot see what a script would.
func exitCodeOf(t *testing.T, opts harnessOptions, args ...string) (*harness, int) {
	t.Helper()
	h, cmd := build(t, opts, args...)
	return h, execute(context.Background(), cmd, h.streams)
}

// TestUsageErrorsExitTwo checks that what cobra and pflag reject is a usage
// error. Each of these exited 1, which a wrapper reads as "some nodes are
// unhealthy", and a misspelt subcommand below the root printed its group's
// help and exited 0, so that "&& clusterctl bmc power off" went ahead.
func TestUsageErrorsExitTwo(t *testing.T) {
	tests := [][]string{
		{"bmc", "power", "off", "--bogus", "-n", "exe0001"},
		{"bmc", "web"},
		{"--fanout", "abc", "version"},
		{"bmcx"},
		{"bmc", "power", "off", "-n"},
		{"version", "extra"},
		{"-o", "bogus", "version"},
		{"slurm", "node", "drian", "ticket 42", "-n", "exe0007", "-y"},
		{"slurm", "node", "drian", "x", "-n", "exe0001", "-y"},
		{"-o", "json", "slurm", "node", "drian", "x", "-n", "exe0001", "-y"},
		{"bmc", "powr", "off", "-n", "exe0001", "-y"},
		{"provision", "reinstal", "-n", "exe0001", "-y"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, code := exitCodeOf(t, harnessOptions{}, args...)
			if code != exitcode.Usage {
				t.Errorf("exit code = %d, want %d; stderr:\n%s", code, exitcode.Usage, h.errOut)
			}
			if h.out.Len() != 0 {
				t.Errorf("a usage error printed to stdout, where a script reads results:\n%s", h.out)
			}
			if calls := h.recorder.Calls(); len(calls) != 0 {
				t.Errorf("a usage error sent %d requests", len(calls))
			}
		})
	}
}

// TestUnknownSubcommandIsNamed checks that the refusal says which word was
// not understood and suggests what was probably meant.
func TestUnknownSubcommandIsNamed(t *testing.T) {
	h, code := exitCodeOf(t, harnessOptions{}, "slurm", "node", "drian", "x", "-n", "exe0001", "-y")
	if code != exitcode.Usage {
		t.Fatalf("exit code = %d, want %d", code, exitcode.Usage)
	}
	for _, want := range []string{`unknown command "drian" for "clusterctl slurm node"`, "drain"} {
		if !strings.Contains(h.errOut.String(), want) {
			t.Errorf("stderr does not say %q:\n%s", want, h.errOut)
		}
	}
}

// TestGroupWithoutArgumentsPrintsHelp checks that a group on its own still
// lists its subcommands and succeeds.
func TestGroupWithoutArgumentsPrintsHelp(t *testing.T) {
	for _, args := range [][]string{{"slurm", "node"}, {}} {
		h, code := exitCodeOf(t, harnessOptions{}, args...)
		if code != exitcode.OK {
			t.Errorf("%q: exit code = %d, want 0; stderr:\n%s", args, code, h.errOut)
		}
		if !strings.Contains(h.out.String(), "Available Commands") {
			t.Errorf("%q: no help was printed:\n%s", args, h.out)
		}
	}
}
