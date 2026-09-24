// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package tunnel_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

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

	path := m.PIDFile("ipmi")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A process id left behind by a crash must not be reported as a running
	// tunnel. 0 never names a process that can be signalled.
	if err := os.WriteFile(path, []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Status() {
		if s.Name == "ipmi" && s.Running {
			t.Error("a stale process id file was reported as running")
		}
	}

	// The running process of this test does answer.
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range m.Status() {
		if s.Name == "ipmi" {
			found = s.Running
		}
	}
	if !found {
		t.Error("a live process was not reported as running")
	}
}

func TestStopReportsATunnelThatIsNotRunning(t *testing.T) {
	t.Parallel()

	if err := manager(t).Stop("ipmi"); err == nil {
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
