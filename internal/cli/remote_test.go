// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// sshTimedOut is what the transport returns when ssh exits 255: classify has
// already coded it as a transport failure.
func sshTimedOut(tg transport.Target) *transport.Result {
	detail := "ssh: connect to host " + tg.Host + " port 22: Connection timed out"
	return &transport.Result{
		Target:   tg,
		ExitCode: 255,
		Stderr:   detail + "\n",
		Err:      exitcode.Wrap(exitcode.Transport, fmt.Errorf("%s: %s", tg, detail)),
	}
}

const dhcpdConf = `host exe0007 {
  hardware ethernet aa:bb:cc:00:00:07;
  fixed-address %s;
}
host exe0002 {
  hardware ethernet aa:bb:cc:00:00:02;
  fixed-address %s;
}
`

// dhcpServer answers cat of dhcpd.conf with the host blocks of one site,
// and reports every boot link it is asked to set as set.
func dhcpServer(prefix string) func(transport.Target, transport.Request) (*transport.Result, error) {
	return func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		switch {
		case len(req.Argv) > 0 && req.Argv[0] == "cat":
			return &transport.Result{Target: tg,
				Stdout: fmt.Sprintf(dhcpdConf, prefix+".7", prefix+".2")}, nil
		case strings.Contains(req.Script, "bootlink 0"):
			return &transport.Result{Target: tg, Stdout: "ok\t0\n"}, nil
		}
		return &transport.Result{Target: tg}, nil
	}
}

// Report 2.13: a dry run read dhcpd.conf from the recorder, got nothing and
// failed for a node whose address comes from DHCP.
func TestDryRunReadsDHCPForReal(t *testing.T) {
	rec := &transport.Recorder{Reply: dhcpServer("10.0.2")}
	h, err := run(t, harnessOptions{recorder: rec}, "boot", "set", "-n", "exe0007", "--dry-run")
	if err != nil {
		t.Fatalf("boot set --dry-run failed: %v\n%s", err, h.errOut)
	}
	if !strings.Contains(h.errOut.String(), "Would set the network boot configuration of 1 host: exe0007") {
		t.Errorf("the preview is missing:\n%s", h.errOut)
	}
	read := false
	for _, call := range rec.Calls() {
		if isBootPathCheck(call.Request) {
			continue
		}
		if len(call.Request.Argv) == 0 || call.Request.Argv[0] != "cat" {
			t.Errorf("a dry run sent %q", call.Command)
			continue
		}
		read = read || call.Target.Role == "dhcp"
	}
	if !read {
		t.Error("the dry run did not read dhcpd.conf from the DHCP server")
	}
}

// Report 5.10: the cached dhcpd.conf of one site answered another site's
// lookup, because the key held only the role name.
func TestDHCPCacheIsKeptPerTarget(t *testing.T) {
	cache := t.TempDir()

	siteA := &transport.Recorder{Reply: dhcpServer("10.0.2")}
	if _, err := run(t, harnessOptions{recorder: siteA, cacheDir: cache}, "dhcp", "hosts", "-n", "exe0002"); err != nil {
		t.Fatalf("dhcp hosts failed: %v", err)
	}

	siteB := &transport.Recorder{Reply: dhcpServer("10.99.0")}
	h, err := run(t, harnessOptions{recorder: siteB, cacheDir: cache},
		"--set", "hosts.dhcp.host=dhcp02.other.org", "boot", "set", "-y", "-n", "exe0002")
	if err != nil {
		t.Fatalf("boot set failed: %v", err)
	}
	asked := false
	for _, call := range siteB.Calls() {
		if call.Target.Host == "dhcp02.other.org" {
			asked = true
		}
		if strings.Contains(call.Command, "10.0.2.2") {
			t.Errorf("site B was sent site A's address: %q", call.Command)
		}
	}
	if !asked {
		t.Error("site B's DHCP server was never asked; the other site's cache answered")
	}
	if strings.Contains(h.out.String(), "10.0.2.2") {
		t.Errorf("output shows site A's address:\n%s", h.out)
	}
}

// The RemoteFile half of report 11.2: a cat that fails on a host that
// answered is that host's failure, and what it said is kept.
func TestDHCPReadFailureKeepsTheMessage(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{
			Target: tg, ExitCode: 1,
			Stderr: "cat: /etc/dhcp/dhcpd.conf: No such file or directory\n",
			Err:    fmt.Errorf("%s: command exited 1", tg),
		}, nil
	}}
	_, err := run(t, harnessOptions{recorder: rec}, "dhcp", "hosts", "-n", "exe0002")
	if err == nil {
		t.Fatal("a failed read succeeded")
	}
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(err.Error(), "No such file or directory") {
		t.Errorf("error %q lost what cat said", err)
	}

	_, err = run(t, harnessOptions{recorder: &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return sshTimedOut(tg), nil
	}}}, "dhcp", "hosts", "-n", "exe0002")
	if got, want := exitcode.From(err), exitcode.Transport; got != want {
		t.Errorf("exit code for an unreachable host = %d, want %d", got, want)
	}
}
