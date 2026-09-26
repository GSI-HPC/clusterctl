// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// doctorChecks runs doctor with -o json and returns its checks by name.
func doctorChecks(t *testing.T, opts harnessOptions, args ...string) (map[string]check, *harness) {
	t.Helper()
	h, _ := run(t, opts, append([]string{"doctor", "-o", "json"}, args...)...)
	var checks []check
	if err := json.Unmarshal(h.out.Bytes(), &checks); err != nil {
		t.Fatalf("doctor -o json did not print JSON: %v\n%s", err, h.out)
	}
	out := map[string]check{}
	for _, c := range checks {
		out[c.Name] = c
	}
	return out, h
}

// TestDoctorChecksTheConfiguredIPMIBackend is the report's 12.11: ipmipower
// was looked for whatever the back end, ipmitool never, and a configured
// path was looked up by its name in PATH.
func TestDoctorChecksTheConfiguredIPMIBackend(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "fping", "exit 0\n")
	fakeTool(t, bin, "ipmipower", "exit 0\n")
	rec, _ := shellRunner(t, bin, "mgmt")

	checks, _ := doctorChecks(t, harnessOptions{recorder: rec}, "--remote",
		"--set", "bmc.ipmi.backend=ipmitool",
		"--set", "bmc.ipmi.ipmitoolPath=/opt/ipmi/bin/ipmitool")
	got := checks["tools on mgmt"]
	if got.Status != statusFail || !strings.Contains(got.Detail, "/opt/ipmi/bin/ipmitool") {
		t.Errorf("tools on mgmt = %+v, want the configured ipmitool missing", got)
	}
	if strings.Contains(got.Detail, "ipmipower") {
		t.Errorf("ipmipower was looked for although the back end is ipmitool: %+v", got)
	}
}

func TestDoctorFindsTheBackendAtItsPath(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "fping", "exit 0\n")
	fakeTool(t, bin, "ipmitool", "exit 0\n")
	rec, _ := shellRunner(t, bin, "mgmt")

	checks, _ := doctorChecks(t, harnessOptions{recorder: rec}, "--remote",
		"--set", "bmc.ipmi.backend=ipmitool",
		"--set", "bmc.ipmi.ipmitoolPath="+bin+"/ipmitool")
	if got := checks["tools on mgmt"]; got.Status != statusOK {
		t.Errorf("tools on mgmt = %+v, want ok", got)
	}
}

// TestDoctorDryRunChecksTheRoles: under --dry-run every role was reported
// as not contacted, although the check only runs true. It is a read, so a
// dry run makes it for real: a role that answers is ok, and one that does
// not fails as it would without --dry-run.
func TestDoctorDryRunChecksTheRoles(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []transport.Request
	)
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		mu.Lock()
		sent = append(sent, req)
		mu.Unlock()
		if tg.Role == "dhcp" {
			return &transport.Result{Target: tg, ExitCode: 255, Err: exitcode.Errorf(exitcode.Transport, "connection refused")}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	checks, _ := doctorChecks(t, harnessOptions{recorder: rec}, "--remote", "--dry-run")
	roles := 0
	for name, c := range checks {
		if !strings.HasPrefix(name, "role ") {
			continue
		}
		roles++
		want := statusOK
		if name == "role dhcp" {
			want = statusFail
		}
		if c.Status != want {
			t.Errorf("%s = %+v under --dry-run, want %s", name, c, want)
		}
	}
	if roles < 2 {
		t.Errorf("%d roles are named in the output, want every role of the example", roles)
	}
	for _, req := range sent {
		if len(req.Argv) > 0 && req.Argv[0] != "true" {
			t.Errorf("doctor sent %q, which is not a check", req.Argv)
		}
		if req.Script != "" && !strings.Contains(req.Script, "command -v") && !strings.Contains(req.Script, "test -x") {
			t.Errorf("doctor sent a script that is not a check:\n%s", req.Script)
		}
	}
}

// TestDoctorOpensTheIdentities is the report's 12.11: a directory passed the
// stat the identity check made.
func TestDoctorOpensTheIdentities(t *testing.T) {
	dir := t.TempDir()
	checks, _ := doctorChecks(t, harnessOptions{}, "--set", `workstation.identities=["`+dir+`"]`)
	got := checks["age identities"]
	if got.Status != statusFail || !strings.Contains(got.Detail, "directory") {
		t.Errorf("age identities = %+v, want a directory refused", got)
	}
}

// TestDoctorHonoursTheOutputFormatWithoutAConfiguration is the report's
// 12.11: doctor -o json printed a table when the configuration did not load.
func TestDoctorHonoursTheOutputFormatWithoutAConfiguration(t *testing.T) {
	checks, _ := doctorChecks(t, harnessOptions{bare: true, config: []string{t.TempDir()}})
	if got := checks["configuration"]; got.Status != statusFail {
		t.Errorf("configuration = %+v, want failed", got)
	}
}

// TestDoctorReportsWhatSSHSaysAboutItsConfiguration is the report's 6.12: a
// misspelt option breaks every connection, and ssh names it only on a line
// before the last.
func TestDoctorReportsWhatSSHSaysAboutItsConfiguration(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "ssh", `
case "$1" in
-V) echo "OpenSSH_9.6p1" >&2 ;;
-G) echo "$3: line 12: Bad configuration option: forwardagnet" >&2
    echo "$3: terminating, 1 bad configuration options" >&2
    exit 255 ;;
esac
`)
	checks, _ := doctorChecks(t, harnessOptions{}, "--set", "ssh.binary="+bin+"/ssh")
	got := checks["generated ssh configuration"]
	if got.Status != statusFail || !strings.Contains(got.Detail, "Bad configuration option: forwardagnet") {
		t.Errorf("generated ssh configuration = %+v, want the bad option named", got)
	}
}

// doctor asked ssh for its version outside the command's context, so a
// client that hung there held doctor after Ctrl-C until it chose to answer.
func TestDoctorStopsAskingSSHWhenInterrupted(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "ssh", "exec sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)

	start := time.Now()
	h, _ := run(t, harnessOptions{ctx: ctx}, "doctor", "--set", "ssh.binary="+bin+"/ssh")
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("doctor took %v to return after the interrupt:\n%s", elapsed, h.out)
	}
}

// doctorRemoteRows returns the checks of the roles doctor --remote made, in
// the order it printed them, one "name | status | detail" line each.
func doctorRemoteRows(t *testing.T, h *harness) string {
	t.Helper()
	var checks []check
	if err := json.Unmarshal(h.out.Bytes(), &checks); err != nil {
		t.Fatalf("doctor -o json did not print JSON: %v\n%s", err, h.out)
	}
	var rows []string
	for _, c := range checks {
		if strings.HasPrefix(c.Name, "role ") || strings.HasPrefix(c.Name, "tools on ") {
			rows = append(rows, c.Name+" | "+c.Status+" | "+c.Detail)
		}
	}
	return strings.Join(rows, "\n") + "\n"
}

// doctor --remote asked each role twice, whether it answered and then for
// its tools, one role after the other. Each role is asked once now, all of
// them side by side, and the checks come in the order of the roles
// whatever order the answers came in: a role that answers last is listed
// where it was, one that could not be reached fails with why, one that
// lacks a tool says which, and one whose checks ran out of time answered
// but is warned about.
func TestDoctorRemoteAsksEachRoleOnce(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		mu.Lock()
		asked[tg.Role]++
		mu.Unlock()
		switch tg.Role {
		case "db":
			time.Sleep(100 * time.Millisecond)
		case "dhcp":
			return transport.ExitResult(tg, 255, "", "ssh: connect to host dhcp01.example.org port 22: Connection refused\n"), nil
		case "fabric":
			return &transport.Result{Target: tg, Stdout: "ibqueryerrors\nperfquery\n"}, nil
		case "install":
			return &transport.Result{Target: tg, ExitCode: 124, Err: &transport.TimeoutError{Target: tg, ExitCode: 124, Timeout: 30 * time.Second}}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "doctor", "--remote", "-o", "json")
	wantCode(t, err, exitcode.TargetFailed)
	want := `role db | ok | dbm01.hpc.example.org
role dhcp | failed | dhcp (dhcp01.example.org): ssh: connect to host dhcp01.example.org port 22: Connection refused
role fabric | ok | ibgw01.example.org
tools on fabric | failed | missing: ibqueryerrors, perfquery
role ifs | ok | ifs01.hpc.example.org
role install | ok | installer.hpc.example.org
tools on install | warning | the checks ran out of time after 30s; the programs were not all looked for
role login | ok | login.hpc.example.org
tools on login | ok | 6 programs present
role mgmt | ok | mgmt-gw.example.org
tools on mgmt | ok | 2 programs present
role mirror | ok | mirror.hpc.example.org
role pool | ok | pool.example.org
role tftp | ok | tftp.example.org
role wlm | ok | wlm01.hpc.example.org
`
	if got := doctorRemoteRows(t, h); got != want {
		t.Errorf("checks:\n%s\nwant:\n%s", got, want)
	}
	for role, n := range asked {
		if n != 1 {
			t.Errorf("role %s was asked %d times, want once", role, n)
		}
	}
}

// A role with tools answered only when its session ran there: one whose
// account logs in but runs nothing, one whose session the interrupt ended
// and one ssh was stopped for here fail as a role without tools would, and
// only checks that ran out of time on the host read the role as answered.
func TestDoctorRemoteFailsARoleWhoseToolsSessionDidNotRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result func(tg transport.Target) *transport.Result
		want   string
	}{
		{"an account that runs nothing", func(tg transport.Target) *transport.Result {
			return transport.ExitResult(tg, 1, "", "This account is currently not available.\n")
		}, "role login | failed | login (login.hpc.example.org): command exited 1"},
		{"an interrupt", func(tg transport.Target) *transport.Result {
			return &transport.Result{Target: tg, ExitCode: 255, Err: exitcode.Wrap(exitcode.Interrupted, context.Canceled)}
		}, "role login | failed | context canceled"},
		{"ssh stopped here", func(tg transport.Target) *transport.Result {
			return &transport.Result{Target: tg, ExitCode: -1,
				Err: exitcode.Wrap(exitcode.Transport, fmt.Errorf("the host stopped answering: %w", context.DeadlineExceeded))}
		}, "role login | failed | the host stopped answering: context deadline exceeded"},
		{"checks that ran out of time", func(tg transport.Target) *transport.Result {
			return &transport.Result{Target: tg, ExitCode: 124, Err: &transport.TimeoutError{Target: tg, ExitCode: 124, Timeout: 30 * time.Second}}
		}, "role login | ok | login.hpc.example.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
				if tg.Role == "login" {
					return tc.result(tg), nil
				}
				return &transport.Result{Target: tg}, nil
			}}
			h, _ := run(t, harnessOptions{recorder: rec}, "doctor", "--remote", "-o", "json")
			if rows := doctorRemoteRows(t, h); !strings.Contains(rows, tc.want) {
				t.Errorf("checks:\n%s\nwant a row that reads %q", rows, tc.want)
			}
		})
	}
}

// At most fanout.PerHost sessions go to one host at once, and a session to
// a host behind a jump host counts on the jump host too, since it opens a
// connection there as well: six roles on one gateway and three behind it
// never have more than four sessions on it, while the others go on beside
// them. Each session is held until one more than the bound are under way,
// which never happens while the bound is kept.
func TestDoctorRemoteKeepsToFourSessionsAHost(t *testing.T) {
	site := exampleWith(t, "site.yaml", func(s string) string {
		var roles strings.Builder
		for _, r := range []string{"gwa", "gwb", "gwc", "gwd", "gwe", "gwf"} {
			fmt.Fprintf(&roles, "    %s:\n      host: gw.example.org\n", r)
		}
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(&roles, "    behind%d:\n      host: b%d.example.org\n      proxyJump: gwa\n", i, i)
		}
		return strings.Replace(s, "  hosts:\n", "  hosts:\n"+roles.String(), 1)
	})
	onGateway := &fanouttest.InFlight{Hold: fanout.PerHost + 1}
	others := &fanouttest.InFlight{}
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if strings.HasPrefix(tg.Role, "gw") || strings.HasPrefix(tg.Role, "behind") {
			defer onGateway.Enter()()
		} else {
			defer others.Enter()()
		}
		return &transport.Result{Target: tg}, nil
	}}
	h, _ := run(t, harnessOptions{config: []string{site}, recorder: rec}, "doctor", "--remote", "-o", "json")
	if got := onGateway.Peak(); got != fanout.PerHost {
		t.Errorf("%d sessions were open on gw.example.org at once, want %d", got, fanout.PerHost)
	}
	if got := onGateway.Started(); got != 9 {
		t.Errorf("%d sessions went to gw.example.org or through it, want 9", got)
	}
	if got := others.Started(); got != 11 {
		t.Errorf("%d sessions went to the other roles, want 11", got)
	}
	if rows := doctorRemoteRows(t, h); !strings.Contains(rows, "role behind3 | ok | b3.example.org\n") {
		t.Errorf("the roles behind the gateway were not checked:\n%s", rows)
	}
}

// Once doctor is interrupted no role is asked any more, and each role it
// did not get to is listed as failed, with the interrupt as the reason.
func TestDoctorRemoteAsksNothingOnceInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		cancel()
		return &transport.Result{Target: tg}, nil
	}}
	h, _ := run(t, harnessOptions{ctx: ctx, recorder: rec}, "--fanout", "1", "doctor", "--remote", "-o", "json")
	if calls := rec.Calls(); len(calls) != 1 {
		t.Errorf("%d roles were asked, want only the one under way when the interrupt came", len(calls))
	}
	rows := doctorRemoteRows(t, h)
	if !strings.Contains(rows, "role wlm | failed | wlm (wlm01.hpc.example.org): context canceled\n") {
		t.Errorf("the roles left out are not listed as interrupted:\n%s", rows)
	}
}
