// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/app"
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

// TestDefaultRolePrefersLoginThenMgmt: login without a name goes to the login
// role, else to mgmt, else to the first role in name order, and names none
// when the site has no role.
func TestDefaultRolePrefersLoginThenMgmt(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  string
	}{
		{"login and mgmt", []string{"wlm", "mgmt", "login", "db"}, "login"},
		{"mgmt without login", []string{"wlm", "mgmt", "db"}, "mgmt"},
		{"neither", []string{"wlm", "install", "db"}, "db"},
		{"no role", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &app.App{}
			a.Spec.Hosts = map[string]v1alpha1.HostRole{}
			for _, role := range tt.roles {
				a.Spec.Hosts[role] = v1alpha1.HostRole{Host: role + ".example.org"}
			}
			if got := defaultRole(a); got != tt.want {
				t.Errorf("defaultRole of %v = %q, want %q", tt.roles, got, tt.want)
			}
		})
	}
}

// TestLoginReachesTheNodeANameSelects: login exe1 connected to
// exe1.hpc.example.org, while exec -n exe1 reaches exe0001, because a bare
// name that was not a role went to the naming rules as it was written.
func TestLoginReachesTheNodeANameSelects(t *testing.T) {
	for _, name := range []string{"exe1", "EXE0001", "exe0001"} {
		t.Run(name, func(t *testing.T) {
			h, err := run(t, harnessOptions{}, "--dry-run", "login", name)
			if err != nil {
				t.Fatalf("login %s failed: %v", name, err)
			}
			if want := "-- alice_adm@exe0001.hpc.example.org"; !strings.Contains(h.out.String(), want) {
				t.Errorf("login %s would run:\n%s\nwant it to reach %q", name, h.out, want)
			}
		})
	}
}
