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
			wantCode(t, err, exitcode.Usage)
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

// An empty node set argument is refused as an empty -n is. It was read as
// no argument and replaced by the session set, so that with
// CLUSTERCTL_NODES=@rack:R02, exec "" -- uptime ran on ten hosts, and with
// CLUSTERCTL_NODES=exe0001, boot grub unset "" -y removed the GRUB link of
// exe0001.
func TestAnEmptyNodeSetArgumentDoesNotFallBackToTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env  string
		args []string
	}{
		{"@rack:R02", []string{"exec", "", "--", "uptime"}},
		{"@rack:R02", []string{"exec", "--dry-run", " ", "--", "uptime"}},
		{"@rack:R02", []string{"exec", "", "", "--", "uptime"}},
		{"@rack:R02", []string{"hostkey", "remove", "", "--dry-run"}},
		{"@rack:R02", []string{"bmc", "power", "off", "", "--dry-run"}},
		{"@rack:R02", []string{"boot", "set", "", "-y"}},
		{"@rack:R02", []string{"boot", "status", ""}},
		{"@rack:R02", []string{"node", "list", ""}},
		{"@rack:R02", []string{"slurm", "node", "list", ""}},
		{"@rack:R02", []string{"slurm", "job", "list", " "}},
		{"exe0001", []string{"boot", "grub", "unset", "", "-y"}},
		{"exe0001", []string{"boot", "grub", "set", "", "/srv/tftp/grub/1.0/grub.cfg.install-exec", "-y"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Setenv(config.EnvNodes, tc.env)
			t.Setenv("BMC_PASSWORD", "s3cret")
			h, err := run(t, harnessOptions{tty: true}, tc.args...)
			if err == nil {
				t.Fatalf("an empty argument should be refused; it printed:\n%s%s", h.out, h.errOut)
			}
			wantCode(t, err, exitcode.Usage)
			if !strings.Contains(err.Error(), "empty") && !strings.Contains(err.Error(), "no node was named") {
				t.Errorf("error = %v, want it to say the argument is empty", err)
			}
			if strings.Contains(h.errOut.String(), "Would") {
				t.Errorf("a preview was printed for an empty argument:\n%s", h.errOut)
			}
			wantNoCalls(t, h)
		})
	}
}

// Without -n the session set still applies.
func TestTheEnvironmentAppliesWithoutNodesFlag(t *testing.T) {
	t.Setenv(config.EnvNodes, "@rack:R02")
	// A dry run of bmc power resolves the BMC account, as the real run does.
	t.Setenv("BMC_PASSWORD", "s3cret")

	h, err := run(t, harnessOptions{recorder: slurmAllIdle(t)}, "bmc", "power", "off", "--dry-run")
	if err != nil {
		t.Fatalf("a dry run on the session set failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "10 hosts") {
		t.Errorf("the dry run does not act on the session set:\n%s", h.errOut)
	}
}

// A repeated -n is refused rather than all but the last being dropped.
func TestTheNodesFlagIsGivenOnce(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"bmc", "power", "off", "-n", "exe0001", "-n", "exe0002", "--dry-run"},
		{"bmc", "power", "off", "-n", "exe0001", "-n", "", "--dry-run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, args...)
			if err == nil {
				t.Fatalf("a repeated -n should be refused; it printed:\n%s%s", h.out, h.errOut)
			}
			wantCode(t, err, exitcode.Usage)
			if strings.Contains(h.errOut.String(), "Would") {
				t.Errorf("a preview was printed:\n%s", h.errOut)
			}
		})
	}
}

// A node set given both as an argument and with -n is refused rather than
// the argument silently replacing -n.
func TestANodeSetArgumentAndTheNodesFlagContradict(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"bmc", "power", "off", "-n", "exe0001", "exe0002", "--dry-run"},
		{"bmc", "power", "off", "-n", "exe0001", "exe0002", "-y"},
		{"node", "list", "-n", "exe0001", "exe0002"},
		{"slurm", "node", "drain", "ticket 4711", "exe0002", "-n", "exe0001", "--dry-run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{tty: true}, args...)
			if err == nil {
				t.Fatalf("an argument together with -n should be refused; it printed:\n%s%s", h.out, h.errOut)
			}
			wantCode(t, err, exitcode.Usage)
			if !strings.Contains(err.Error(), "-n") {
				t.Errorf("error = %v, want it to name -n", err)
			}
			if strings.Contains(h.errOut.String(), "Would") {
				t.Errorf("a preview was printed:\n%s", h.errOut)
			}
			wantNoCalls(t, h)
		})
	}
}

// An argument replaces the session set, which is only a default.
func TestANodeSetArgumentWinsOverTheEnvironment(t *testing.T) {
	t.Setenv(config.EnvNodes, "@rack:R02")
	// A dry run of bmc power resolves the BMC account, as the real run does.
	t.Setenv("BMC_PASSWORD", "s3cret")

	h, err := run(t, harnessOptions{recorder: slurmAllIdle(t)}, "bmc", "power", "off", "exe0002", "--dry-run")
	if err != nil {
		t.Fatalf("a dry run on an argument failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "1 host: exe0002") {
		t.Errorf("the dry run does not act on the argument alone:\n%s", h.errOut)
	}
}
