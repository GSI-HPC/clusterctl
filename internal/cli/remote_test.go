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
		case req.Script == bootLinkScript:
			return &transport.Result{Target: tg, Stdout: reportLinks(req)}, nil
		}
		return &transport.Result{Target: tg}, nil
	}
}

// Report 2.13: a dry run read dhcpd.conf from the recorder, got nothing and
// failed for a node whose address comes from DHCP.
func TestDryRunReadsDHCPForReal(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	wantCode(t, err, exitcode.TargetFailed)
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

// boot status goes on past a node whose address could not be read, and every
// node without an inventory address read the DHCP configuration again: a
// server that could not be reached was asked once per node, an ssh timeout
// each, and a good one was fetched once per node once its cached copy had
// expired. The command reads it once.
func TestBootStatusReadsDHCPOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		reply func(transport.Target, transport.Request) (*transport.Result, error)
		code  int
	}{
		{"a server that answers", dhcpServer("10.0.2"), exitcode.OK},
		{"a server that cannot be reached", func(tg transport.Target, req transport.Request) (*transport.Result, error) {
			if tg.Role == "dhcp" {
				return sshTimedOut(tg), nil
			}
			return &transport.Result{Target: tg}, nil
		}, exitcode.Transport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &transport.Recorder{Reply: tc.reply}
			h, err := run(t, harnessOptions{recorder: rec},
				"--set", "services.dhcp.cacheTtl=1ns", "boot", "status", "-n", "exe0002,exe0007")
			if got := exitcode.From(err); got != tc.code {
				t.Errorf("exit code = %d (%v), want %d\n%s", got, err, tc.code, h.out)
			}
			reads := 0
			for _, c := range rec.Calls() {
				if c.Target.Role == "dhcp" {
					reads++
				}
			}
			if reads != 1 {
				t.Errorf("the DHCP server was asked %d times, want once", reads)
			}
		})
	}
}

// The commands that only read went through the runner a dry run records
// to, so a dry run read nothing and showed it: boot list listed no boot
// configuration, the logs were empty, and fabric counters --uplink found
// no switch port linked to the node. They read through the transport in a
// dry run too, as a lookup does, and show what the host answered.
func TestReadingCommandsReadInADryRun(t *testing.T) {
	t.Parallel()
	answers := map[string]string{
		"find":          "/srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2\n",
		"tail":          "Sep 30 10:00:01 pxe pxesrv[42]: exe0001 fetched ipxe.net2\n",
		"ibaddr":        "GID fe80::11:2203:33:4455 LID start 0x5 end 0x5\n",
		"iblinkinfo":    `0x0002c90200404ad8 "SwitchX -  Mellanox Technologies"      4   17[  ] ==( 4X 25.78 Gbps Active/  LinkUp)==>  0x0011220300334455      5    1[  ] "exe0001 HCA-1" ( )` + "\n",
		"perfquery":     "# Port counters: Lid 4 port 17\nSymbolErrorCounter:..............0\n",
		"ibqueryerrors": "Errors for 0x0011220300334455\n",
	}
	scripts := map[string]string{
		"dnsmasq-tftp": "Sep 30 10:00:02 tftp in.tftpd[7]: RRQ from 10.0.2.1 filename grub.cfg\n",
		"dhcpd":        "Sep 30 10:00:00 dhcp01 dhcpd[9]: DHCPACK on 10.0.2.1 to aa:bb:cc:00:00:01\n",
	}
	reply := func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if len(req.Argv) == 0 {
			return &transport.Result{Target: tg}, nil
		}
		if req.Argv[0] == "sh" {
			for word, out := range scripts {
				if strings.Contains(strings.Join(req.Argv, " "), word) {
					return &transport.Result{Target: tg, Stdout: out}, nil
				}
			}
		}
		if req.Argv[0] == "cat" {
			return &transport.Result{Target: tg, Stdout: "host exe0001 {\n  hardware ethernet 00:11:22:33:44:55;\n  option dhcp-client-identifier = ff:00:00:00:00:00:02:00:00:02:c9:00:00:11:22:03:00:33:44:55;\n}\n"}, nil
		}
		return &transport.Result{Target: tg, Stdout: answers[req.Argv[0]]}, nil
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"boot", "list"}, "ipxe.net2"},
		{[]string{"boot", "log"}, "fetched ipxe.net2"},
		{[]string{"boot", "grub", "log"}, "RRQ from 10.0.2.1"},
		{[]string{"dhcp", "log"}, "DHCPACK on 10.0.2.1"},
		{[]string{"fabric", "counters", "exe0001"}, "Errors for 0x0011220300334455"},
		{[]string{"fabric", "counters", "exe0001", "--uplink"}, "SymbolErrorCounter"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{recorder: &transport.Recorder{Reply: reply}}, append([]string{"--dry-run"}, tc.args...)...)
			if err != nil {
				t.Fatalf("%v --dry-run failed: %v\n%s", tc.args, err, h.errOut)
			}
			if !strings.Contains(h.out.String(), tc.want) {
				t.Errorf("%v --dry-run printed:\n%s\nwant %q", tc.args, h.out, tc.want)
			}
		})
	}
}
