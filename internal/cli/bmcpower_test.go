// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// noSlurm turns the Slurm job check off, which these tests are not about.
var noSlurm = []string{"--set", "safety.slurmAware=false"}

// ipmiCalls returns the recorded IPMI runs.
func ipmiCalls(h *harness) []transport.Call {
	var out []transport.Call
	for _, c := range h.recorder.Calls() {
		if strings.Contains(c.Request.Script, "--hostname") || strings.Contains(c.Request.Script, "chassis power") {
			out = append(out, c)
		}
	}
	return out
}

// jsonRows decodes the one JSON value a command printed.
func jsonRows(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &rows); err != nil {
		t.Fatalf("the output is not one JSON array: %v\n%s", err, h.out)
	}
	return rows
}

// A misspelt action went to Slurm, and over IPMI to the credential, and then
// exited 3 as though a host could not be reached; the dry run approved it.
func TestBMCPowerChecksTheActionFirst(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	for _, args := range [][]string{
		{"bmc", "power", "offf", "--ipmi", "-y", "-n", "exe0001"},
		{"bmc", "power", "offf", "-y", "-n", "exe0001"},
		{"--dry-run", "bmc", "power", "offf", "-n", "exe0001"},
		// reboot was accepted over Redfish only, and is in no list.
		{"bmc", "power", "reboot", "-y", "-n", "exe0001"},
	} {
		h, err := run(t, harnessOptions{}, args...)
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("%v: exit code %d, want %d (%v)", args, got, exitcode.Usage, err)
		}
		if calls := h.recorder.Calls(); len(calls) != 0 {
			t.Errorf("%v: sent %d commands before refusing the action: %v", args, len(calls), h.recorder.Commands())
		}
	}
}

// An IPMI backend that timeout(1) stopped after one answer made the command
// exit 0 with one row for four nodes.
func TestBMCPowerOverIPMIReportsEveryNode(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	recorder := &transport.Recorder{Reply: func(target transport.Target, req transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: target, Stdout: "exe0001.mgmt.hpc.example.org: ok\n", ExitCode: 124}, nil
	}}
	h, err := run(t, harnessOptions{recorder: recorder},
		append(noSlurm, "-o", "json", "bmc", "power", "off", "--ipmi", "-y", "-n", "exe[0001-0004]")...)
	if got := exitcode.From(err); got != exitcode.Transport {
		t.Errorf("exit code %d, want %d (%v)", got, exitcode.Transport, err)
	}
	rows := jsonRows(t, h)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4:\n%s", len(rows), h.out)
	}
	for _, row := range rows[1:] {
		if row["error"] == nil {
			t.Errorf("%v was never reported but has no error", row["node"])
		}
	}
}

// Only a power-on was batched; a cycle of a rack drew its inrush current at
// once. Batch 8 over ten nodes is sent as 5 and 5, as the manual says.
func TestBMCPowerCycleIsBatched(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	h, err := run(t, harnessOptions{recorder: ipmiOK()},
		append(noSlurm, "bmc", "power", "cycle", "--ipmi", "--stagger", "1ms", "-y", "-n", "exe[0001-0010]")...)
	if err != nil {
		t.Fatalf("bmc power cycle failed: %v\n%s", err, h.errOut)
	}
	calls := ipmiCalls(h)
	if len(calls) != 2 {
		t.Fatalf("got %d IPMI runs, want 2: %v", len(calls), h.recorder.Commands())
	}
	for _, c := range calls {
		if got := len(ipmiRequestHosts(c.Request)); got != 5 {
			t.Errorf("a batch of %d, want 5: %s", got, c.Command)
		}
	}
}

// A power-on stopped at the first batch with a failure and never mentioned
// the rest; each batch printed its own JSON array; an explicit zero was
// taken for no flag.
func TestBMCPowerOnReportsEveryBatch(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	recorder := &transport.Recorder{Reply: ipmiAnswer(func(bmc string) string {
		if strings.HasPrefix(bmc, "exe0002.") {
			return "connection timeout"
		}
		return "ok"
	})}
	h, err := run(t, harnessOptions{recorder: recorder},
		append(noSlurm, "-o", "json", "bmc", "power", "on", "--ipmi", "--batch", "3", "--stagger", "1ms", "-y", "-n", "exe[1-10]")...)
	if err == nil {
		t.Fatal("a failed batch was not reported")
	}
	if !strings.Contains(err.Error(), "1 of 10 service processors failed, 7 not tried: exe[0004-0010]") {
		t.Errorf("error = %v, want it to count the untried nodes and name them", err)
	}
	rows := jsonRows(t, h)
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10:\n%s", len(rows), h.out)
	}
	if got := rows[9]["state"]; got != "not tried" {
		t.Errorf("exe0010 state = %v, want not tried", got)
	}

	// Without failures the whole run is one JSON value.
	h, err = run(t, harnessOptions{recorder: ipmiOK()},
		append(noSlurm, "-o", "json", "bmc", "power", "on", "--ipmi", "--batch", "3", "--stagger", "0", "-y", "-n", "exe[1-10]")...)
	if err != nil {
		t.Fatalf("power on failed: %v", err)
	}
	if rows := jsonRows(t, h); len(rows) != 10 {
		t.Errorf("got %d rows, want 10", len(rows))
	}
	// --stagger 0 means no pause, not the configured 5s.
	if strings.Contains(h.errOut.String(), "waiting") {
		t.Errorf("--stagger 0 still paused:\n%s", h.errOut)
	}
	if got := len(ipmiCalls(h)); got != 4 {
		t.Errorf("got %d batches, want 4", got)
	}

	// An explicit --batch below 1 is refused like safety.powerOnBatch, not
	// replaced by the configuration and not read as the whole set at once.
	h, err = run(t, harnessOptions{recorder: ipmiOK()},
		append(noSlurm, "bmc", "power", "on", "--ipmi", "--batch", "0", "-y", "-n", "exe[1-10]")...)
	if got := exitcode.From(err); got != exitcode.Usage {
		t.Errorf("--batch 0: exit code %d, want %d (%v)", got, exitcode.Usage, err)
	}
	if got := len(ipmiCalls(h)); got != 0 {
		t.Errorf("--batch 0: %d batches were sent, want none", got)
	}
}

// One backend was built from the first node's vendor profile, so every
// processor in the set was offered that vendor's account.
func TestBMCPowerGroupsTheSetByAccount(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	t.Setenv("V1_PASSWORD", "v1pass")
	site := exampleWith(t, "site.yaml", func(s string) string {
		s = strings.Replace(s, "  credentials:\n",
			"  credentials:\n    bmc-v1:\n      username: admin1\n      password:\n        fromEnv: V1_PASSWORD\n", 1)
		s = strings.Replace(s, "    vendors:\n", "    vendors:\n      vendor1:\n        credential: bmc-v1\n        order: [ipmi]\n", 1)
		return strings.Replace(s, "backend: ipmipower", "backend: ipmitool", 1)
	})
	args := append(noSlurm, "--set", "bmc.order=[ipmi]", "bmc", "power", "off", "--force", "-y", "-n", "dbm01,exe0005")

	h, err := run(t, harnessOptions{config: []string{site}, recorder: ipmiOK()}, args...)
	if err != nil {
		t.Fatalf("bmc power failed: %v\n%s", err, h.errOut)
	}
	calls := ipmiCalls(h)
	if len(calls) != 2 {
		t.Fatalf("got %d IPMI runs, want one per account: %v", len(calls), h.recorder.Commands())
	}
	for _, c := range calls {
		hosts := strings.Join(ipmiRequestHosts(c.Request), ",")
		switch {
		case strings.Contains(c.Command, "admin1"):
			if hosts != "dbm01.mgmt.hpc.example.org" {
				t.Errorf("vendor1's account went to %s", hosts)
			}
		default:
			if hosts != "exe0005.mgmt.hpc.example.org" {
				t.Errorf("the site's account went to %s", hosts)
			}
		}
	}

	// The preview names each group.
	h, err = run(t, harnessOptions{config: []string{site}}, append([]string{"--dry-run"}, args...)...)
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if out := h.errOut.String() + h.out.String(); !strings.Contains(out, "bmc-v1") || !strings.Contains(out, "exe0005") {
		t.Errorf("the preview does not show the groups:\n%s", out)
	}
}

// A misspelt transport chose Redfish; a node without a Redfish service
// failed instead of falling back to IPMI; bmc power status ignored the order.
func TestBMCOrderIsCheckedAndFallsBack(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")

	_, err := run(t, harnessOptions{}, "--dry-run", "--set", "bmc.order=[impi]", "bmc", "power", "off", "-n", "exe0001")
	if got := exitcode.From(err); got != exitcode.Usage {
		t.Errorf("bmc.order=[impi]: exit code %d, want %d (%v)", got, exitcode.Usage, err)
	}

	// Nothing listens for Redfish on 127.0.0.1, so the connection is
	// refused and the request provably never reached the processor.
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0001\n      bmcAddress: 127.0.0.1\n"
	})
	for _, args := range [][]string{
		{"bmc", "status", "-n", "exe0001"},
		{"bmc", "power", "status", "-n", "exe0001"},
		{"bmc", "power", "off", "-y", "-n", "exe0001"},
	} {
		h, err := run(t, harnessOptions{config: []string{inventory}, recorder: ipmiOK()}, append(noSlurm, args...)...)
		if err != nil {
			t.Errorf("%v: failed instead of falling back to IPMI: %v\n%s%s", args, err, h.out, h.errOut)
			continue
		}
		if got := ipmiHosts(h); len(got) != 1 || got[0] != "127.0.0.1" {
			t.Errorf("%v: IPMI runs named %v, want 127.0.0.1", args, got)
		}
	}

	// bmc power status follows the order.
	h, err := run(t, harnessOptions{recorder: ipmiOK()}, "--set", "bmc.order=[ipmi]", "bmc", "power", "status", "-n", "exe0001")
	if err != nil {
		t.Fatalf("bmc power status failed: %v", err)
	}
	if len(ipmiCalls(h)) != 1 {
		t.Errorf("bmc power status did not use IPMI first: %v", h.recorder.Commands())
	}
}

// A missing credential was one row among the results and exit 1, where the
// IPMI path exited 2; a processor that refused the connection exited 1.
func TestBMCFailuresKeepTheirExitCode(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "")
	if err := os.Unsetenv("BMC_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"bmc", "power", "off", "-y", "-n", "exe0001"},
		{"bmc", "status", "-n", "exe0001"},
		{"-o", "json", "bmc", "redfish", "info", "-n", "exe0001"},
		{"bmc", "boot", "show", "-n", "exe0001"},
	} {
		h, err := run(t, harnessOptions{}, append(noSlurm, args...)...)
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("%v without a password: exit code %d, want %d (%v)\n%s", args, got, exitcode.Usage, err, h.out)
		}
	}

	t.Setenv("BMC_PASSWORD", "s3cret")
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0001\n      bmcAddress: 127.0.0.1\n"
	})
	redfishOnly := []string{"--set", "bmc.order=[redfish]"}
	for _, args := range [][]string{
		{"bmc", "status", "-n", "exe0001"},
		{"bmc", "power", "off", "-y", "-n", "exe0001"},
		{"bmc", "boot", "set", "Pxe", "-y", "-n", "exe0001"},
		{"bmc", "boot", "show", "-n", "exe0001"},
		{"bmc", "redfish", "get", "/redfish/v1", "-n", "exe0001"},
	} {
		_, err := run(t, harnessOptions{config: []string{inventory}}, append(append(noSlurm, redfishOnly...), args...)...)
		if got := exitcode.From(err); got != exitcode.Transport {
			t.Errorf("%v against a refused connection: exit code %d, want %d (%v)", args, got, exitcode.Transport, err)
		}
	}

	// bmc redfish info names every node in its object, the failed ones with
	// their error.
	h, err := run(t, harnessOptions{config: []string{inventory}},
		append(redfishOnly, "-o", "json", "bmc", "redfish", "info", "-n", "exe[0001-0002]")...)
	if got := exitcode.From(err); got != exitcode.Transport {
		t.Errorf("redfish info: exit code %d, want %d (%v)", got, exitcode.Transport, err)
	}
	var object map[string]map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &object); err != nil {
		t.Fatalf("redfish info printed no JSON object: %v\n%s", err, h.out)
	}
	if object["exe0001"]["error"] == nil {
		t.Errorf("exe0001 is missing its error: %v", object)
	}
}

// An interrupt before the fan-out reported every node as failed with exit 1,
// although nothing could have been sent.
func TestBMCInterruptBeforeSendingExits130(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")
	ctx, cancel := context.WithCancel(context.Background())
	// The interrupt arrives while the confirmation is being given.
	recorder := &transport.Recorder{Reply: func(target transport.Target, _ transport.Request) (*transport.Result, error) {
		cancel()
		return &transport.Result{Target: target, Stdout: "exe0001 idle\nexe0002 idle\nexe0003 idle\n"}, nil
	}}
	defer cancel()
	h, err := run(t, harnessOptions{ctx: ctx, recorder: recorder},
		"-o", "json", "bmc", "power", "off", "-y", "-n", "exe[0001-0003]")
	if got := exitcode.From(err); got != exitcode.Interrupted {
		t.Errorf("exit code %d, want %d (%v)", got, exitcode.Interrupted, err)
	}
	if strings.Contains(h.out.String(), "state") {
		for _, row := range jsonRows(t, h) {
			if row["state"] != "not sent" {
				t.Errorf("%v: state %v, want not sent", row["node"], row["state"])
			}
		}
	}
}
