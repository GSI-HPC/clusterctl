// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// TestDanglingOperatorIsAUsageError covers a set operator left without its
// right operand. It is what a command substitution that printed nothing leaves
// behind: "@rack:R02&$(clusterctl slurm node nodeset idle)" with no idle node
// used to select the whole rack.
func TestDanglingOperatorIsAUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"node", "select", "@rack:R02&", "--expand"},
		{"node", "select", "exe[1-10]!,exe5"},
		{"node", "select", "exe[1-10]&&exe5"},
		{"bmc", "power", "off", "--dry-run", "-n", "@rack:R02&"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, args...)
			if err == nil {
				t.Fatalf("the command succeeded:\n%s%s", h.out, h.errOut)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d (%v)", got, want, err)
			}
			if !strings.Contains(err.Error(), "operand") {
				t.Errorf("error = %v, want it to name the missing operand", err)
			}
			if strings.Contains(h.errOut.String(), "Would") {
				t.Errorf("a preview was printed:\n%s", h.errOut)
			}
		})
	}
}
