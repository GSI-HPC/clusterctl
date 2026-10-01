// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

package e2e

import "testing"

// TestDoctorReachesTheRoles checks that doctor --remote reaches every role
// of the site and finds the Slurm clients on the login role, which is what
// the slurm commands need of it.
func TestDoctorReachesTheRoles(t *testing.T) {
	t.Parallel()
	r := clusterctl(t, "doctor", "--remote", "-o", "json")
	r.wantCode(t, 0)
	checks := map[string]string{}
	for _, c := range decode[[]struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}](t, r) {
		checks[c.Name] = c.Status
		if c.Status == "failed" {
			t.Errorf("the check %q failed: %s", c.Name, c.Detail)
		}
	}
	for _, name := range []string{"role login", "tools on login", "role wlm"} {
		if checks[name] != "ok" {
			t.Errorf("the check %q is %q, want ok: %s", name, checks[name], r)
		}
	}
}
