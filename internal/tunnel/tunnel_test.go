// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package tunnel_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/tunnel"
)

func manager(t *testing.T) *tunnel.Manager {
	t.Helper()
	return &tunnel.Manager{
		Profiles: map[string]v1alpha1.TunnelSpec{
			"ipmi": {
				Remote:   "mgmt",
				Subnets:  []string{"ipmi"},
				Excludes: []string{"{workstation.host}"},
			},
			"internal": {
				Remote:  "pool",
				Subnets: []string{"internal", "10.20.0.0/16"},
				DNS:     true,
				User:    "alice",
				Method:  "nft",
				Options: []string{"--verbose"},
			},
			"broken": {Remote: "mgmt", Subnets: []string{"nowhere"}},
			"empty":  {Remote: "mgmt"},
		},
		Networks: map[string]string{"ipmi": "10.0.0.0/8", "internal": "10.10.0.0/16"},
		StateDir: t.TempDir(),
		Vars:     map[string]string{"workstation.host": "desk01.example.org", "nowhere": ""},
		Destination: func(remote, user string) (string, error) {
			host := remote
			switch remote {
			case "mgmt":
				host = "mgmt-gw.example.org"
			case "pool":
				host = "pool.example.org"
				if user == "" {
					user = "root"
				}
			}
			if user == "" {
				user = "admin"
			}
			return user + "@" + host, nil
		},
		SSH: func() ([]string, error) {
			return []string{"ssh", "-F", "/state dir/ssh_config-0123456789abcdef"}, nil
		},
	}
}

func TestNames(t *testing.T) {
	t.Parallel()

	if got, want := strings.Join(manager(t).Names(), ","), "broken,empty,internal,ipmi"; got != want {
		t.Errorf("Names = %q, want %q", got, want)
	}
}

func TestArgsResolvesNamesAndTemplates(t *testing.T) {
	t.Parallel()

	args, err := manager(t).Args("ipmi")
	if err != nil {
		t.Fatalf("Args failed: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"sshuttle", "--daemon", "--pidfile", "--remote admin@mgmt-gw.example.org",
		"--exclude desk01.example.org", "10.0.0.0/8",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args are missing %q: %s", want, joined)
		}
	}
}

func TestArgsCarriesEveryOption(t *testing.T) {
	t.Parallel()

	args, err := manager(t).Args("internal")
	if err != nil {
		t.Fatalf("Args failed: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--remote alice@pool.example.org", "--dns", "--method nft", "--verbose",
		"10.10.0.0/16", "10.20.0.0/16",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args are missing %q: %s", want, joined)
		}
	}
}

func TestArgsReportsProblems(t *testing.T) {
	t.Parallel()
	m := manager(t)

	if _, err := m.Args("nope"); err == nil {
		t.Error("an unknown tunnel should be reported")
	} else if !strings.Contains(err.Error(), "ipmi") {
		t.Errorf("error = %v, want it to list the configured tunnels", err)
	}
	if _, err := m.Args("empty"); err == nil {
		t.Error("a tunnel routing nothing should be reported")
	}
	// A subnet that resolves to nothing would silently route everything or
	// nothing, so it stops the command instead.
	if _, err := m.Args("broken"); err == nil {
		t.Error("a subnet that resolves to nothing should be reported")
	}
}

func TestStatusIgnoresAStalePIDFile(t *testing.T) {
	t.Parallel()
	m := manager(t)

	// A process id left behind by a crash must not be reported as a running
	// tunnel.
	writePIDFile(t, m.PIDFile("ipmi"), 2147483646)
	if running(m, "ipmi") {
		t.Error("a stale process id file was reported as running")
	}

	// Nor may the process that has since been given the same number: this
	// test's own process answers, but it is not the tunnel.
	writePIDFile(t, m.PIDFile("ipmi"), os.Getpid())
	if running(m, "ipmi") {
		t.Error("a process that is not the tunnel was reported as running")
	}
}

func running(m *tunnel.Manager, name string) bool {
	for _, s := range m.Status() {
		if s.Name == name {
			return s.Running
		}
	}
	return false
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

// fakeSshuttle writes a program that behaves as sshuttle --daemon does for
// the process id file: it refuses to start while the file exists, and
// leaves a process behind, still carrying its arguments, whose id it writes
// there.
func fakeSshuttle(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the fake relies on /proc to be found")
	}
	path := filepath.Join(t.TempDir(), "sshuttle")
	script := `#!/bin/sh
for a; do [ "$prev" = --pidfile ] && pidfile=$a; prev=$a; done
[ -e "$pidfile" ] && { echo "$pidfile: sshuttle is already running" >&2; exit 1; }
( trap 'exit 0' TERM; while :; do sleep 0.1; done ) </dev/null >/dev/null 2>&1 &
echo $! >"$pidfile"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// gone waits for a process to end, or to be a zombie nobody reaps yet.
func gone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if err != nil || len(data) == 0 {
			return true
		}
	}
	return false
}

// A tunnel whose process id file now names another process is not running,
// and starting it replaces the file rather than leaving sshuttle to refuse.
// Stopping it then ends the tunnel and nothing else.
func TestStartStopFindTheTunnelByItsProcessIDFile(t *testing.T) {
	t.Parallel()
	m := manager(t)
	m.Binary = fakeSshuttle(t)

	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = other.Wait(); close(done) }()
	t.Cleanup(func() { _ = other.Process.Kill(); <-done })
	writePIDFile(t, m.PIDFile("ipmi"), other.Process.Pid)

	if err := m.Start(context.Background(), "ipmi"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	data, err := os.ReadFile(m.PIDFile("ipmi"))
	if err != nil {
		t.Fatalf("the tunnel wrote no process id file: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	if pid == other.Process.Pid || !running(m, "ipmi") {
		t.Fatalf("the started tunnel, process %d, is not reported as running", pid)
	}
	if err := m.Start(context.Background(), "ipmi"); err == nil {
		t.Error("a tunnel that is running was started again")
	}

	if err := m.Stop(context.Background(), "ipmi"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if !gone(pid) {
		t.Error("Stop left the tunnel running")
	}
	select {
	case <-done:
		t.Error("Start or Stop ended a process that is not the tunnel")
	default:
	}
}

func TestStopRefusesAnUnknownName(t *testing.T) {
	t.Parallel()
	m := manager(t)

	outside := filepath.Join(m.StateDir, "victim.pid")
	writePIDFile(t, outside, os.Getpid())
	if err := m.Stop(context.Background(), "../victim"); err == nil || !strings.Contains(err.Error(), "unknown tunnel") {
		t.Errorf("Stop(../victim) = %v, want it refused as an unknown tunnel", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("Stop removed a file outside the tunnels directory: %v", err)
	}
}

func TestStopReportsATunnelThatIsNotRunning(t *testing.T) {
	t.Parallel()

	if err := manager(t).Stop(context.Background(), "ipmi"); err == nil {
		t.Error("stopping a tunnel that is not running should be reported")
	}
}

func TestStatusDescribesEveryProfile(t *testing.T) {
	t.Parallel()

	status := manager(t).Status()
	if got, want := len(status), 4; got != want {
		t.Fatalf("got %d entries, want %d", got, want)
	}
	for _, s := range status {
		if s.Name == "" || s.Remote == "" {
			t.Errorf("entry is incomplete: %+v", s)
		}
	}
}

// sshuttle runs whatever --ssh-cmd names, split as a POSIX shell would.
func TestArgsConnectsWithTheGivenSSHCommand(t *testing.T) {
	t.Parallel()

	args, err := manager(t).Args("ipmi")
	if err != nil {
		t.Fatalf("Args failed: %v", err)
	}
	i := slices.Index(args, "--ssh-cmd")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("args have no --ssh-cmd: %q", args)
	}
	if got, want := args[i+1], "ssh -F '/state dir/ssh_config-0123456789abcdef'"; got != want {
		t.Errorf("--ssh-cmd = %q, want %q", got, want)
	}

	m := manager(t)
	m.SSH = nil
	if _, err := m.Args("ipmi"); err == nil {
		t.Error("a tunnel was built to connect with plain ssh")
	}
}

// The ssh command and the process id file are clusterctl's to set; a
// profile's options come before them, and one that sets them is refused.
func TestArgsRefusesOptionsClusterctlSets(t *testing.T) {
	t.Parallel()

	for _, option := range []string{
		"-e", "-essh", "--ssh-cmd=ssh", "--ssh", "--ssh-c",
		"-r", "--remote", "--remote=elsewhere", "--pidfile=/tmp/x", "--pid", "-D", "--daemon",
	} {
		m := manager(t)
		profile := m.Profiles["ipmi"]
		profile.Options = []string{option, "value"}
		m.Profiles["ipmi"] = profile
		if _, err := m.Args("ipmi"); err == nil {
			t.Errorf("the option %q was accepted", option)
		}
	}
	for _, option := range []string{"--remote-shell=sh", "--python", "-v", "--dns", "-x"} {
		m := manager(t)
		profile := m.Profiles["ipmi"]
		profile.Options = []string{option}
		m.Profiles["ipmi"] = profile
		args, err := m.Args("ipmi")
		if err != nil {
			t.Errorf("the option %q was refused: %v", option, err)
			continue
		}
		if slices.Index(args, option) > slices.Index(args, "--ssh-cmd") {
			t.Errorf("the option %q comes after --ssh-cmd: %q", option, args)
		}
	}
}

// An exclude that expands to nothing would route what it was meant to keep
// off the tunnel.
func TestArgsRefusesAnEmptyExclude(t *testing.T) {
	t.Parallel()
	m := manager(t)
	m.Vars["workstation.host"] = ""

	if _, err := m.Args("ipmi"); err == nil || !strings.Contains(err.Error(), "{workstation.host}") {
		t.Errorf("Args = %v, want the empty exclude reported", err)
	}
}
