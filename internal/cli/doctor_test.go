// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
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
