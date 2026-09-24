// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// An explicit -n is final. Given empty, it is a usage error and the session
// set in CLUSTERCTL_NODES is not read in its place, so that
// -n "$(clusterctl slurm node nodeset drain)" with nothing drained reaches
// no host rather than all of them.
func TestAnEmptyNodesFlagDoesNotFallBackToTheEnvironment(t *testing.T) {
	t.Setenv(config.EnvNodes, "@rack:R02")

	for _, args := range [][]string{
		{"exec", "--dry-run", "-n", "", "--", "uptime"},
		{"bmc", "power", "off", "--dry-run", "-n", ""},
		{"bmc", "power", "off", "--dry-run", "--nodes="},
		{"bmc", "power", "off", "--dry-run", "-n", " "},
		{"bmc", "power", "off", "-y", "-n", ""},
		{"slurm", "node", "list", "-n", ""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{tty: true}, args...)
			if err == nil {
				t.Fatalf("an empty -n should be refused; it printed:\n%s%s", h.out, h.errOut)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d (%v)", got, want, err)
			}
			if !strings.Contains(err.Error(), "-n") {
				t.Errorf("error = %v, want it to name -n", err)
			}
			if strings.Contains(h.errOut.String(), "Would") {
				t.Errorf("a preview was printed for an empty -n:\n%s", h.errOut)
			}
			if n := len(h.recorder.Calls()); n != 0 {
				t.Errorf("an empty -n sent %d commands, want none", n)
			}
		})
	}
}

// Without -n the session set still applies.
func TestTheEnvironmentAppliesWithoutNodesFlag(t *testing.T) {
	t.Setenv(config.EnvNodes, "@rack:R02")

	h, err := run(t, harnessOptions{}, "bmc", "power", "off", "--dry-run")
	if err != nil {
		t.Fatalf("a dry run on the session set failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "10 hosts") {
		t.Errorf("the dry run does not act on the session set:\n%s", h.errOut)
	}
}

// A repeated -n is refused rather than all but the last being dropped.
func TestTheNodesFlagIsGivenOnce(t *testing.T) {
	for _, args := range [][]string{
		{"bmc", "power", "off", "-n", "exe0001", "-n", "exe0002", "--dry-run"},
		{"bmc", "power", "off", "-n", "exe0001", "-n", "", "--dry-run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, args...)
			if err == nil {
				t.Fatalf("a repeated -n should be refused; it printed:\n%s%s", h.out, h.errOut)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d (%v)", got, want, err)
			}
			if strings.Contains(h.errOut.String(), "Would") {
				t.Errorf("a preview was printed:\n%s", h.errOut)
			}
		})
	}
}
