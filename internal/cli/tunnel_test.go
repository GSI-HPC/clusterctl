// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	argv := shellWords(t, strings.TrimSpace(h.out.String()))
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
	ssh := shellWords(t, option("--ssh-cmd"))
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

// shellWords splits a command line into the words a POSIX shell makes of it,
// by asking one.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	out, err := exec.Command("sh", "-c", `eval "set -- $1" && printf '%s\0' "$@"`, "sh", line).Output()
	if err != nil {
		t.Fatalf("%q is not one shell command line: %v", line, err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// sleeper starts a process of this user that is not a tunnel.
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd
}

// alive reports whether a process started by sleeper still runs. Waiting on
// it in the background reaps it, so a signalled one does not linger as a
// zombie that still answers signal 0.
func alive(cmd *exec.Cmd) bool {
	time.Sleep(100 * time.Millisecond)
	return cmd.Process.Signal(syscall.Signal(0)) == nil
}

func writePIDFile(t *testing.T, path string, pid int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A process id file outlives a crash, and the number in it can later belong
// to another process of the same user. That process is not the tunnel: it is
// neither reported as one nor stopped.
func TestTunnelStopLeavesAnUnrelatedProcessAlone(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	victim := sleeper(t)
	writePIDFile(t, filepath.Join(state, "tunnels", "ipmi.pid"), victim.Process.Pid)

	h, err := run(t, harnessOptions{stateDir: state}, "tunnel", "status", "-o", "json")
	if err != nil {
		t.Fatalf("tunnel status failed: %v", err)
	}
	if strings.Contains(h.out.String(), `"running": true`) {
		t.Errorf("an unrelated process was reported as a running tunnel:\n%s", h.out)
	}

	if _, err := run(t, harnessOptions{stateDir: state}, "tunnel", "stop", "ipmi"); err == nil {
		t.Error("stopping a tunnel whose process id names another process succeeded")
	}
	if !alive(victim) {
		t.Fatal("tunnel stop signalled an unrelated process")
	}
	if _, err := os.Stat(filepath.Join(state, "tunnels", "ipmi.pid")); err == nil {
		t.Error("the stale process id file was kept")
	}
}

// Only a configured profile is stopped: a name is never a path.
func TestTunnelStopRefusesAnUnknownName(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	victim := sleeper(t)
	outside := filepath.Join(dir, "victim.pid")
	writePIDFile(t, outside, victim.Process.Pid)

	_, err := run(t, harnessOptions{stateDir: state}, "tunnel", "stop", "../../victim")
	if err == nil {
		t.Error("stopping a tunnel the site does not define succeeded")
	} else if !strings.Contains(err.Error(), "unknown tunnel") {
		t.Errorf("error = %v, want it to name the tunnel as unknown", err)
	}
	if !alive(victim) {
		t.Error("tunnel stop signalled the process named in a file outside the tunnels directory")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("tunnel stop removed a file outside the tunnels directory: %v", err)
	}
}

// Without a Workstation document for this machine, {workstation.host} is
// empty. Dropping the exclude would route this machine's own address into the
// tunnel, so the profile is refused instead.
func TestTunnelStartRefusesAnExcludeThatExpandsToNothing(t *testing.T) {
	extra := t.TempDir()
	workstation := "apiVersion: clusterctl/v1alpha1\nkind: Workstation\nspec:\n  sopsKeyTypes: [age]\n"
	if err := os.WriteFile(filepath.Join(extra, "workstation.yaml"), []byte(workstation), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := run(t, harnessOptions{config: []string{extra}}, "tunnel", "start", "ipmi", "--dry-run")
	if err == nil {
		t.Fatalf("a tunnel whose exclude expands to nothing was started:\n%s", h.out)
	}
	if !strings.Contains(err.Error(), "{workstation.host}") {
		t.Errorf("error = %v, want it to name the exclude", err)
	}
}
