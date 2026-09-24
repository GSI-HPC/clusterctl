// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"strings"
	"testing"

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

// TestDoctorDryRunContactsNothing is the report's 2.14: under --dry-run the
// recorder answered every request with success, and every role was reported
// ok without being contacted.
func TestDoctorDryRunContactsNothing(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		t.Errorf("%s was contacted under --dry-run", tg)
		return &transport.Result{Target: tg}, nil
	}}
	checks, _ := doctorChecks(t, harnessOptions{recorder: rec}, "--remote", "--dry-run")
	roles := 0
	for name, c := range checks {
		if !strings.HasPrefix(name, "role ") && !strings.HasPrefix(name, "tools on ") {
			continue
		}
		roles++
		if c.Status != statusSkip {
			t.Errorf("%s = %+v under --dry-run, want it skipped", name, c)
		}
	}
	if roles == 0 {
		t.Error("the roles are not named in the output")
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
