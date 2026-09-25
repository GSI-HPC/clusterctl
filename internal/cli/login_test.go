// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// TestLoginRejectsASecondName: login used to take the first word as the name
// and drop the rest, so "login mgmt uptime" opened a shell instead of
// running uptime.
func TestLoginRejectsASecondName(t *testing.T) {
	for _, args := range [][]string{
		{"login", "mgmt", "uptime"},
		{"login", "mgmt", "extra", "--", "uptime"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, append([]string{"--dry-run"}, args...)...)
			if err == nil {
				t.Fatalf("a second name should be refused; would run:\n%s", h.out)
			}
			wantCode(t, err, exitcode.Usage)
			if !strings.Contains(err.Error(), "--") {
				t.Errorf("error = %v, want it to point at --", err)
			}
		})
	}

	h, err := run(t, harnessOptions{}, "--dry-run", "login", "mgmt", "--", "uptime")
	if err != nil {
		t.Fatalf("login with a command failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "uptime") {
		t.Errorf("the command is missing:\n%s", h.out)
	}
}
