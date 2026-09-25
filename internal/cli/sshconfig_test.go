// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// The generated ssh configuration is what every connection is made with, so
// these tests read it the way ssh does: ssh -G prints what ssh settles on for
// a host after reading the file and everything it includes.

// sshHome gives the test a home directory of its own, so that the
// administrator's ~/.ssh/config, which the example site includes, cannot
// change what ssh resolves.
func sshHome(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("the OpenSSH client is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// generatedSSHConfig runs a dry-run login and returns the -F file it names.
func generatedSSHConfig(t *testing.T, opts harnessOptions, args ...string) string {
	t.Helper()
	args = append(args, "login", "--dry-run", "install", "--", "true")
	h, err := run(t, opts, args...)
	if err != nil {
		t.Fatalf("login --dry-run failed: %v\n%s", err, h.errOut)
	}
	fields := strings.Fields(h.out.String())
	if len(fields) < 3 || fields[1] != "-F" {
		t.Fatalf("login --dry-run printed no -F file:\n%s", h.out)
	}
	return fields[2]
}

func sshResolve(t *testing.T, config, host string) map[string]string {
	t.Helper()
	out, err := exec.Command("ssh", "-G", "-F", config, host).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G -F %s %s failed: %v\n%s", config, host, err, out)
	}
	values := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if key, value, ok := strings.Cut(line, " "); ok {
			values[key] = value
		}
	}
	return values
}

func writeTestFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 6.1: a FreeIPA or sssd client adds a global host key file and a
// KnownHostsCommand through /etc/ssh/ssh_config.d. Neither may vouch for a
// host: the site's file is the only trust anchor.
func TestSSHChecksHostKeysAgainstTheSiteFileAlone(t *testing.T) {
	sshHome(t)
	ipa := writeTestFile(t, filepath.Join(t.TempDir(), "04-ipa.conf"),
		"GlobalKnownHostsFile /var/lib/sss/pubconf/known_hosts\n"+
			"KnownHostsCommand /usr/bin/sss_ssh_knownhosts %H\n"+
			"VerifyHostKeyDNS yes\nUpdateHostKeys yes\n")
	config := generatedSSHConfig(t, harnessOptions{}, "--set", "ssh.include=["+ipa+"]")

	got := sshResolve(t, config, "exe0001.hpc.example.org")
	site, err := filepath.Abs(filepath.Join(exampleDir, "ssh-known-hosts"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		// 6.12: the harness passes a relative --config, and the file must
		// still name the site's file wherever ssh is run from.
		"userknownhostsfile":    site,
		"globalknownhostsfile":  "/dev/null",
		"stricthostkeychecking": "true",
		"verifyhostkeydns":      "false",
		"updatehostkeys":        "false",
	} {
		if got[key] != want {
			t.Errorf("ssh resolved %s %q, want %q", key, got[key], want)
		}
	}
	if cmd := got["knownhostscommand"]; cmd != "" {
		t.Errorf("an included KnownHostsCommand is in effect: %q", cmd)
	}
}

// 6.2: the state directory is per user. Another context, --config tree or
// --set must never rewrite the file a running process connects with.
func TestEachConfigurationGetsItsOwnSSHConfig(t *testing.T) {
	state := t.TempDir()
	first := generatedSSHConfig(t, harnessOptions{stateDir: state}, "--context", "cluster1")
	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	second := generatedSSHConfig(t, harnessOptions{stateDir: state},
		"--context", "cluster2",
		"--set", "ssh.strictHostKeyChecking=false",
		"--set", "ssh.knownHostsFile=/other/site/known_hosts")
	if first == second {
		t.Fatalf("both runs connect with %s", first)
	}
	after, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the second run rewrote the first run's configuration:\n%s", after)
	}
	if strings.Contains(string(after), "accept-new") || strings.Contains(string(after), "/other/site") {
		t.Errorf("the first run's configuration carries the second run's trust settings:\n%s", after)
	}
}

// 6.4: a YAML block description is valid, and must not end its comment line.
func TestAMultiLineRoleDescriptionKeepsSSHWorking(t *testing.T) {
	sshHome(t)
	config := generatedSSHConfig(t, harnessOptions{},
		"--set", `hosts.mgmt.description="Management gateway\nStrictHostKeyChecking no"`)
	if got := sshResolve(t, config, "mgmt-gw.example.org")["stricthostkeychecking"]; got != "true" {
		t.Errorf("a line of the description became a directive: StrictHostKeyChecking %q", got)
	}
}

func TestALineBreakInAnSSHValueIsAUsageError(t *testing.T) {
	for _, set := range []string{
		`hosts.wlm.user="root\nStrictHostKeyChecking no"`,
		`hosts.wlm.host="wlm01.hpc.example.org\nUser root"`,
		`ssh.options={Compression: "yes\nStrictHostKeyChecking no"}`,
		`ssh.knownHostsFile="/site/known_hosts\nStrictHostKeyChecking no"`,
	} {
		_, err := run(t, harnessOptions{}, "--set", set, "config", "validate")
		if err == nil {
			t.Errorf("config validate accepted --set %s", set)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("--set %s: exit code %d, want %d (%v)", set, got, want, err)
		}
	}
}

// 6.5: a ControlMaster from an included file would carry node connections
// through a master that checked its host key against something else.
func TestIncludedControlMasterDoesNotReachNodes(t *testing.T) {
	sshHome(t)
	mux := writeTestFile(t, filepath.Join(t.TempDir(), "mux.conf"),
		"ControlMaster auto\nControlPath ~/.ssh/cm-%C\nControlPersist 10m\n")
	config := generatedSSHConfig(t, harnessOptions{}, "--set", "ssh.include=["+mux+"]")

	if got := sshResolve(t, config, "exe0001.hpc.example.org")["controlmaster"]; got != "false" {
		t.Errorf("a compute node multiplexes: ControlMaster %q", got)
	}
	gw := sshResolve(t, config, "mgmt-gw.example.org")
	if gw["controlmaster"] != "auto" {
		t.Errorf("the gateway role lost its multiplexing: ControlMaster %q", gw["controlmaster"])
	}
	if strings.Contains(gw["controlpath"], "/.ssh/") {
		t.Errorf("the gateway uses the included socket %q", gw["controlpath"])
	}
}

// 6.6: with -F ssh reads no user configuration of its own, so the generated
// file includes it.
func TestTheUsersOwnSSHConfigIsInEffect(t *testing.T) {
	home := sshHome(t)
	writeTestFile(t, filepath.Join(home, ".ssh", "config"),
		"Host exe0001.hpc.example.org\n  Port 2222\n  StrictHostKeyChecking no\n")
	config := generatedSSHConfig(t, harnessOptions{})
	got := sshResolve(t, config, "exe0001.hpc.example.org")
	if got["port"] != "2222" {
		t.Errorf("the user's own ssh configuration is not read: port %q", got["port"])
	}
	// It cannot lower the trust settings, which come first.
	if got["stricthostkeychecking"] != "true" {
		t.Errorf("the user's own configuration relaxed host key checking: %q", got["stricthostkeychecking"])
	}
}

// 6.7: ADR 0017 puts the configuration under ~/Library/Application Support
// on macOS. ssh splits an unquoted value on the space.
func TestAHostKeyFileWithASpaceIsWrittenAsOnePath(t *testing.T) {
	known := filepath.Join(t.TempDir(), "Application Support", "clusterctl", "ssh-known-hosts")
	config := generatedSSHConfig(t, harnessOptions{}, "--set", `ssh.knownHostsFile="`+known+`"`)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if want := `UserKnownHostsFile "` + known + `"`; !strings.Contains(string(data), want) {
		t.Errorf("the host key file is not written as one quoted path, want %s:\n%s", want, data)
	}
}

// 6.8: in the example site the wlm role logs in with its own account on
// wlm01, which is also an inventory node. Without a context user, a command
// on the node must not run as the role's account.
func TestANodeNamedLikeARoleDoesNotLogInWithTheRoleAccount(t *testing.T) {
	sshHome(t)
	set := []string{"--set", `defaultUser=""`, "--set", "hosts.wlm.user=slurmadm"}
	h, err := run(t, harnessOptions{}, append(set, "login", "--dry-run", "wlm01", "--", "id")...)
	if err != nil {
		t.Fatalf("login --dry-run failed: %v", err)
	}
	// ssh -F FILE ... -- DESTINATION COMMAND
	fields := strings.Fields(h.out.String())
	dest := ""
	for i, f := range fields {
		if f == "--" && i+1 < len(fields) {
			dest = fields[i+1]
			break
		}
	}
	if len(fields) < 3 || dest == "" {
		t.Fatalf("unexpected output:\n%s", h.out)
	}
	if strings.Contains(dest, "@") {
		t.Fatalf("the node got an account on the command line: %s", dest)
	}
	got := sshResolve(t, fields[2], dest)["user"]
	if got == "slurmadm" {
		t.Errorf("a command on the node wlm01 would run as the wlm role's account")
	}
	if me, err := user.Current(); err == nil && got != me.Username {
		t.Errorf("the node would be logged in to as %q, want the local account %q", got, me.Username)
	}

	// The role still logs in with its own account.
	h, err = run(t, harnessOptions{}, append(set, "login", "--dry-run", "wlm", "--", "id")...)
	if err != nil {
		t.Fatalf("login --dry-run wlm failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "slurmadm@wlm01.hpc.example.org") {
		t.Errorf("the role lost its account:\n%s", h.out)
	}
}

// 6.9: ssh follows a jump cycle by starting hops without end. It is a
// configuration error, found before anything is started.
func TestAProxyJumpCycleIsAUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"--set", "hosts.mgmt.proxyJump=dhcp", "config", "validate"},
		{"--set", "hosts.mgmt.proxyJump=dhcp", "login", "-T", "mgmt", "--", "true"},
		{"--set", "hosts.mgmt.proxyJump=root@dhcp01.example.org", "config", "validate"},
	} {
		_, err := run(t, harnessOptions{}, args...)
		if err == nil {
			t.Errorf("%q was accepted", args)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("%q: exit code %d, want %d", args, got, want)
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Errorf("%q: the error does not name the cycle: %v", args, err)
		}
	}
}

// 6.12: an include is a configured path like any other, resolved against
// the Site directory; a pattern is left for ssh.
func TestSSHIncludesAreResolvedAgainstTheSiteDirectory(t *testing.T) {
	config := generatedSSHConfig(t, harnessOptions{}, "--set", `ssh.include=["ssh_config.d/*.conf"]`)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	site, err := filepath.Abs(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := `Include "` + filepath.Join(site, "ssh_config.d", "*.conf") + `"`; !strings.Contains(string(data), want) {
		t.Errorf("the include is not resolved against the site, want %s:\n%s", want, data)
	}
}

func TestSSHOptionsThatWouldBeIgnoredAreRefused(t *testing.T) {
	for _, set := range []string{
		`hosts.login.options={StrictHostKeyChecking: "no"}`,
		`hosts.login.options={Match: all}`,
		`hosts.login.options={User: root}`,
		`ssh.options={ConnectTimeout: "99"}`,
		`hosts.login.host="*.hpc.example.org"`,
		`hosts.dhcp.proxyJump=mgnt`,
	} {
		_, err := run(t, harnessOptions{}, "--set", set, "config", "validate")
		if err == nil {
			t.Errorf("config validate accepted --set %s", set)
		} else if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("--set %s: exit code %d, want %d", set, got, want)
		}
	}
}
