// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
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
			wantCode(t, err, exitcode.Usage)
			if !strings.Contains(err.Error(), "operand") {
				t.Errorf("error = %v, want it to name the missing operand", err)
			}
			if strings.Contains(h.errOut.String(), "Would") {
				t.Errorf("a preview was printed:\n%s", h.errOut)
			}
		})
	}
}

// TestSelectionKeepsTheInventorySpelling adds exe11 to an inventory that
// otherwise writes exe0001 to exe0010. The set used to show every exe host
// four digits wide, so exec targeted exe0011, a host the inventory does not
// hold.
func TestSelectionKeepsTheInventorySpelling(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(exampleDir, "inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	doc := string(src) + "    - nodes: exe11\n      attributes: {class: exe}\n      rack: R02\n"
	if err := os.WriteFile(filepath.Join(dir, "inventory.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		args []string
		want string
	}{
		{[]string{"node", "select", "exe11"}, "exe11"},
		{[]string{"node", "select", "exe[10-11]"}, "exe[0010,11]"},
		{[]string{"node", "fqdn", "-n", "exe11"}, "exe11.hpc.example.org"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{config: []string{dir}}, tc.args...)
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}
			if got := strings.TrimSpace(h.out.String()); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}

	h, err := run(t, harnessOptions{config: []string{dir}}, "exec", "--dry-run", "-n", "exe11", "--", "uptime")
	if err != nil && !safety.IsDryRun(err) {
		t.Fatalf("exec --dry-run failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "1 host: exe11") || strings.Contains(h.errOut.String(), "exe0011") {
		t.Errorf("the preview does not name exe11 as written:\n%s", h.errOut)
	}
}
