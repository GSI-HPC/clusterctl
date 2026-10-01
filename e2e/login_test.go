// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

package e2e

import "testing"

// TestLoginRunsACommand checks that login reaches a node by its name and a
// role by the role's name, and runs what follows -- there.
func TestLoginRunsACommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"a node by its name", []string{"login", "worker-1", "--", "uname", "-n"}, "worker-1\n"},
		{"the login role by default", []string{"login", "--", "uname", "-n"}, "submitter\n"},
		{"a role by its name", []string{"login", "wlm", "--", "uname", "-n"}, "controller\n"},
		{"the arguments unchanged", []string{"login", "worker-0", "--", "printf", "[%s]", "*", "a  b", "it's"}, "[*][a  b][it's]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := clusterctl(t, tt.args...)
			r.wantCode(t, 0)
			if r.stdout != tt.want {
				t.Errorf("printed %q, want %q", r.stdout, tt.want)
			}
		})
	}
}
