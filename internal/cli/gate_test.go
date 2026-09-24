// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"
	"testing"
)

// One machine named three ways was three targets: three resets at once to
// one service processor, and the command run three times on the node.
func TestOneMachineNamedTwiceIsOneTarget(t *testing.T) {
	names := "exe0001,exe0001.hpc.example.org,exe0001.,EXE1,10.0.2.1,exe0001.mgmt.hpc.example.org"

	h, err := run(t, harnessOptions{}, "node", "select", names)
	if err != nil {
		t.Fatalf("node select failed: %v", err)
	}
	if got := strings.TrimSpace(h.out.String()); got != "exe0001" {
		t.Errorf("node select = %q, want exe0001", got)
	}

	h, err = run(t, harnessOptions{}, "exec", "-y", "-n", names, "--", "uptime")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	if calls := h.recorder.Calls(); len(calls) != 1 {
		t.Errorf("exec ran %d times, want once: %v", len(calls), h.recorder.Commands())
	}

	// The Slurm job check is not what this is about.
	h, err = run(t, harnessOptions{}, "bmc", "power", "cycle", "-n", names, "--dry-run", "--lose-jobs")
	if err != nil {
		t.Fatalf("power cycle --dry-run failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "Would power cycle 1 host: exe0001") {
		t.Errorf("the preview does not count one host:\n%s", h.errOut)
	}
}
