// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// TestCompletionContactsNothing is the report's 12.13: completing -n ran the
// list command of every exec group source over ssh, sinfo on the Slurm host
// in the example, on every press of Tab.
func TestCompletionContactsNothing(t *testing.T) {
	for _, args := range [][]string{
		{"__complete", "node", "list", "-n", ""},
		{"__complete", "node", "select", ""},
		// Cobra parses these flags twice while completing, which read the
		// configuration twice and made -n look repeated.
		{"__complete", "-n", "exe0001", "node", "select", ""},
		{"__complete", "--set", "fanout.max=2", "node", "select", ""},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
				return &transport.Result{Target: tg, Stdout: "main\ndebug\n"}, nil
			}}
			h, err := run(t, harnessOptions{recorder: rec}, args...)
			if err != nil {
				t.Fatalf("completion failed: %v", err)
			}
			for _, c := range rec.Calls() {
				t.Errorf("completion contacted %s: %s", c.Target, c.Command)
			}
			out := h.out.String()
			for _, want := range []string{"@inventory:exe", "@rack:R02", "@static:compute"} {
				if !strings.Contains(out, want) {
					t.Errorf("completion does not offer %s:\n%s", want, out)
				}
			}
			if strings.Contains(out, "@slurm:") {
				t.Errorf("completion offers groups it had to run a command for:\n%s", out)
			}
		})
	}
}
