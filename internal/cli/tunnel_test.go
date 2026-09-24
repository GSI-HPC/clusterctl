// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/shellquote"
)

// A tunnel connects with the generated ssh configuration, so that it checks
// the host key against the site's file and goes through the role's jump
// hosts, and with the context's account, like every other connection.
func TestTunnelStartUsesTheGeneratedSSHConfiguration(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state dir")
	h, err := run(t, harnessOptions{stateDir: state}, "tunnel", "start", "ipmi", "--dry-run")
	if err != nil {
		t.Fatalf("tunnel start --dry-run failed: %v\n%s", err, h.errOut)
	}
	argv, err := shellquote.Split(strings.TrimSpace(h.out.String()))
	if err != nil {
		t.Fatalf("the command is not one shell command line: %v\n%s", err, h.out)
	}
	option := func(name string) string {
		for i, arg := range argv {
			if arg == name && i+1 < len(argv) {
				return argv[i+1]
			}
		}
		t.Fatalf("the command has no %s: %q", name, argv)
		return ""
	}

	if got, want := option("--remote"), "alice_adm@mgmt-gw.example.org"; got != want {
		t.Errorf("--remote = %q, want %q, the context's account", got, want)
	}

	// sshuttle splits the command the way a POSIX shell does.
	ssh, err := shellquote.Split(option("--ssh-cmd"))
	if err != nil {
		t.Fatalf("--ssh-cmd is not a shell command line: %v", err)
	}
	if len(ssh) != 3 || ssh[0] != "ssh" || ssh[1] != "-F" {
		t.Fatalf("--ssh-cmd = %q, want ssh -F and the generated file", ssh)
	}
	config := ssh[2]
	if filepath.Dir(config) != state || !strings.HasPrefix(filepath.Base(config), "ssh_config-") {
		t.Errorf("--ssh-cmd names %s, not the configuration generated in %s", config, state)
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("the file --ssh-cmd names was not written: %v", err)
	}
	for _, want := range []string{"StrictHostKeyChecking yes", "UserKnownHostsFile", "GlobalKnownHostsFile /dev/null"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("%s is missing %q", config, want)
		}
	}
}
