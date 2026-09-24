// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// A line of white space from the gateway panicked, and a missing fping or an
// unreachable gateway was reported as every processor down, exit 1.
func TestBMCPingReadsTheSweepCarefully(t *testing.T) {
	reply := func(r *transport.Result) *transport.Recorder {
		return &transport.Recorder{Responses: []*transport.Result{r}}
	}
	h, err := run(t, harnessOptions{recorder: reply(&transport.Result{
		Stdout: "exe0001.mgmt.hpc.example.org\n   \n", ExitCode: 1,
	})}, "bmc", "ping", "-n", "exe[0001-0002]")
	if got := exitcode.From(err); got != exitcode.TargetFailed {
		t.Errorf("one processor down: exit code %d, want %d (%v)", got, exitcode.TargetFailed, err)
	}
	if command := h.recorder.Commands()[0]; !strings.Contains(command, " -- ") {
		t.Errorf("the host list is not separated from the options: %s", command)
	}

	for _, r := range []*transport.Result{
		{Stderr: "bash: fping: command not found\n", ExitCode: 127},
		{Stderr: "ssh: connect to host mgmt-gw.example.org port 22: No route to host\n", ExitCode: 255,
			Err: exitcode.Errorf(exitcode.Transport, "exit 255")},
	} {
		_, err := run(t, harnessOptions{recorder: reply(r)}, "bmc", "ping", "-n", "exe[0001-0002]")
		if got := exitcode.From(err); got != exitcode.Transport {
			t.Errorf("exit %d: exit code %d, want %d (%v)", r.ExitCode, got, exitcode.Transport, err)
		}
	}
}
