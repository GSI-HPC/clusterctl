// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package tunnel_test

import (
	"os"
	"path/filepath"
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
		Host: func(role string) (string, string, error) {
			switch role {
			case "mgmt":
				return "mgmt-gw.example.org", "", nil
			case "pool":
				return "pool.example.org", "root", nil
			default:
				return role, "", nil
			}
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
		"sshuttle", "--daemon", "--pidfile", "--remote mgmt-gw.example.org",
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
