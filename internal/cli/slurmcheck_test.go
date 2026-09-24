// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// sinfoAnswers is a recorder on which sinfo answers with the given output and
// exit code, and every other command succeeds without output.
func sinfoAnswers(stdout string, code int) *transport.Recorder {
	return &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if isSinfo(req) {
			result := &transport.Result{Target: tg, Stdout: stdout, ExitCode: code}
			if code != 0 {
				result.Err = fmt.Errorf("%s: command exited %d", tg, code)
			}
			return result, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
}

// slurmAllIdle is a recorder on which sinfo reports every node it is asked
// about idle, for the tests of what a power action does once it is allowed.
func slurmAllIdle(t *testing.T) *transport.Recorder {
	t.Helper()
	return &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if !isSinfo(req) {
			return &transport.Result{Target: tg}, nil
		}
		var out strings.Builder
		for i, arg := range req.Argv {
			if arg != "-n" || i+1 == len(req.Argv) {
				continue
			}
			ns, err := nodeset.Parse(req.Argv[i+1])
			if err != nil {
				t.Errorf("sinfo was asked about %q: %v", req.Argv[i+1], err)
				break
			}
			for _, name := range ns.Expand() {
				fmt.Fprintf(&out, "%s idle\n", name)
			}
		}
		return &transport.Result{Target: tg, Stdout: out.String()}, nil
	}}
}

func isSinfo(req transport.Request) bool {
	return len(req.Argv) > 0 && req.Argv[0] == "sinfo"
}

// sinfoCalls and powerCalls count what the recorder was asked to send.
func sinfoCalls(rec *transport.Recorder) (sinfo, other int) {
	for _, c := range rec.Calls() {
		if isSinfo(c.Request) {
			sinfo++
		} else {
			other++
		}
	}
	return sinfo, other
}

// powerOffIPMI runs a confirmed IPMI power-off, which reaches the recorder
// when it goes ahead, so that the test sees whether it was sent.
func powerOffIPMI(t *testing.T, rec *transport.Recorder, extra ...string) (*harness, error) {
	t.Helper()
	t.Setenv("BMC_PASSWORD", "secret")
	args := append([]string{"bmc", "power", "off", "--ipmi", "-y"}, extra...)
	return run(t, harnessOptions{recorder: rec}, args...)
}

// Section 3.1 of the September 2026 review: the check was off on every site
// written by config init, because only the example set safety.slurmAware.
func TestSlurmCheckIsOnForAFreshConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site-config")
	if _, err := run(t, harnessOptions{bare: true, config: []string{dir}},
		"config", "init", dir, "--site", "lab", "--cluster", "alpha", "--domain", "hpc.lab.example"); err != nil {
		t.Fatalf("config init failed: %v", err)
	}

	// config init writes no nodes; give one a service processor, so that the
	// power action gets as far as the job check.
	inventory := filepath.Join(dir, "inventory.yaml")
	data, err := os.ReadFile(inventory)
	if err != nil {
		t.Fatal(err)
	}
	withNode := strings.Replace(string(data), "  nodes: []\n",
		"  nodes:\n    - nodes: node1\n      bmcAddress: 10.9.0.1\n", 1)
	if withNode == string(data) {
		t.Fatalf("the scaffolded inventory has no empty node list:\n%s", data)
	}
	if err := os.WriteFile(inventory, []byte(withNode), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "explain", "safety.slurmAware")
	if err != nil {
		t.Fatalf("config explain safety.slurmAware failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "true") {
		t.Errorf("safety.slurmAware is not on:\n%s", h.out)
	}

	rec := sinfoAnswers("node1 allocated\n", 0)
	_, err = run(t, harnessOptions{bare: true, config: []string{dir}, recorder: rec},
		"bmc", "power", "off", "-n", "node1", "-y")
	if err == nil {
		t.Fatal("a node running a job was powered off on a fresh configuration")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d (%v)", got, want, err)
	}
	if sinfo, _ := sinfoCalls(rec); sinfo != 1 {
		t.Errorf("sinfo was sent %d times, want once", sinfo)
	}
}

// Turning the check off is allowed, but it is said before the question, so
// that nobody takes the preview for a checked one.
func TestSlurmCheckSaysWhenItIsOff(t *testing.T) {
	rec := sinfoAnswers("exe0007 allocated\n", 0)
	h, err := powerOffIPMI(t, rec, "-n", "exe7", "--set", "safety.slurmAware=false")
	if err != nil {
		t.Fatalf("power off with the check turned off failed: %v", err)
	}
	if sinfo, _ := sinfoCalls(rec); sinfo != 0 {
		t.Errorf("sinfo was sent %d times with the check turned off", sinfo)
	}
	if !strings.Contains(h.errOut.String(), "safety.slurmAware") {
		t.Errorf("nothing says the job check was skipped:\n%s", h.errOut)
	}
}

// Section 3.2: draining, failing and flagged states run jobs too, and a state
// the check does not know is not taken for idle.
func TestSlurmCheckRefusesEveryBusyState(t *testing.T) {
	refused := []string{
		"allocated", "mixed", "allocated*", "completing",
		// The review found these let through.
		"draining", "draining*", "draining@", "failing",
		"mixed-", "allocated^", "allocated%", "mixed!", "completing%",
		// Slurm prints fail for a mixed node, and maint and reboot for one
		// whose jobs are completing.
		"fail", "maint", "reboot^",
		// Compound and unknown states.
		"mixed+drain", "idle+completing", "reboot_issued", "unknown", "something_new",
	}
	for _, state := range refused {
		t.Run(state, func(t *testing.T) {
			rec := sinfoAnswers("exe0007 "+state+"\n", 0)
			_, err := powerOffIPMI(t, rec, "-n", "exe7")
			if err == nil {
				t.Fatalf("a node in state %s was powered off", state)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d (%v)", got, want, err)
			}
			if !strings.Contains(err.Error(), "exe0007") || !strings.Contains(err.Error(), "--lose-jobs") {
				t.Errorf("error = %v, want it to name the node and the override", err)
			}
			if _, other := sinfoCalls(rec); other != 0 {
				t.Errorf("the power action was sent for state %s", state)
			}
		})
	}

	idle := []string{"idle", "idle*", "idle~", "drained", "drained*", "down*", "reserved", "powered_down", "idle+drain"}
	for _, state := range idle {
		t.Run(state, func(t *testing.T) {
			rec := sinfoAnswers("exe0007 "+state+"\n", 0)
			if _, err := powerOffIPMI(t, rec, "-n", "exe7"); err != nil {
				t.Fatalf("a node in state %s was refused: %v", state, err)
			}
			if _, other := sinfoCalls(rec); other == 0 {
				t.Errorf("the power action was not sent for state %s", state)
			}
		})
	}
}

// Section 3.5: a node sinfo did not list, or listed without a state, is not
// known to be idle.
func TestSlurmCheckRefusesNodesSlurmDidNotReport(t *testing.T) {
	for name, tc := range map[string]struct{ answer, named string }{
		"no answer":     {"", "exe[0001-0002]"},
		"one missing":   {"exe0001 idle\n", "exe0002"},
		"no state":      {"exe0001 idle\nexe0002\n", "exe0002"},
		"another name":  {"exe1 idle\nexe2 idle\n", "exe[0001-0002]"},
		"another state": {"exe0001 idle\nexe0002 allocated\n", "exe0002"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := sinfoAnswers(tc.answer, 0)
			_, err := powerOffIPMI(t, rec, "-n", "exe[1-2]")
			if err == nil {
				t.Fatal("the power action went ahead for a node Slurm did not report as idle")
			}
			if !strings.Contains(err.Error(), tc.named) {
				t.Errorf("error = %v, want it to name %s", err, tc.named)
			}
			if _, other := sinfoCalls(rec); other != 0 {
				t.Error("the power action was sent")
			}
		})
	}
}

// The check fails closed when Slurm cannot be asked; --lose-jobs is the way
// past it, and says what it skipped.
func TestSlurmCheckRefusesWhenSlurmCannotBeAsked(t *testing.T) {
	rec := sinfoAnswers("", 1)
	_, err := powerOffIPMI(t, rec, "-n", "exe7")
	if err == nil {
		t.Fatal("the power action went ahead although Slurm could not be asked")
	}
	if !strings.Contains(err.Error(), "--lose-jobs") {
		t.Errorf("error = %v, want it to name the override", err)
	}
	if _, other := sinfoCalls(rec); other != 0 {
		t.Error("the power action was sent")
	}

	rec = sinfoAnswers("", 1)
	h, err := powerOffIPMI(t, rec, "-n", "exe7", "--lose-jobs")
	if err != nil {
		t.Fatalf("--lose-jobs did not get past the check: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "--lose-jobs") {
		t.Errorf("nothing says the check was overridden:\n%s", h.errOut)
	}
}

// Section 2.10: --force lifts host protection, not the job check, and the
// protected host is named before the job check runs.
func TestForceDoesNotLoseJobs(t *testing.T) {
	answer := "exe0001 allocated\nwlm01 idle\n"

	rec := sinfoAnswers(answer, 0)
	_, err := powerOffIPMI(t, rec, "-n", "exe0001,wlm01")
	if err == nil || !strings.Contains(err.Error(), "wlm01") || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("error = %v, want the protected host named", err)
	}

	rec = sinfoAnswers(answer, 0)
	_, err = powerOffIPMI(t, rec, "-n", "exe0001,wlm01", "--force")
	if err == nil {
		t.Fatal("--force powered off a node running a job")
	}
	if !strings.Contains(err.Error(), "exe0001") || !strings.Contains(err.Error(), "--lose-jobs") {
		t.Errorf("error = %v, want it to name the node and --lose-jobs", err)
	}
	if strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %v, it should not offer --force for the jobs", err)
	}
	if _, other := sinfoCalls(rec); other != 0 {
		t.Error("the power action was sent")
	}

	rec = sinfoAnswers(answer, 0)
	h, err := powerOffIPMI(t, rec, "-n", "exe0001,wlm01", "--force", "--lose-jobs")
	if err != nil {
		t.Fatalf("--force --lose-jobs was refused: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "exe0001") {
		t.Errorf("the jobs about to be lost are not named:\n%s", h.errOut)
	}

	// The help says which flag gets past which check.
	h, err = run(t, harnessOptions{}, "bmc", "power", "--help")
	if err != nil {
		t.Fatalf("bmc power --help failed: %v", err)
	}
	for _, want := range []string{"--lose-jobs is given", "--force gets past a protected host, not this check"} {
		if !strings.Contains(strings.Join(strings.Fields(h.out.String()), " "), want) {
			t.Errorf("the help does not say %q:\n%s", want, h.out)
		}
	}
}

// Section 2.14: a dry run asks Slurm as the real run would, so that it does
// not preview an action the real run refuses.
func TestSlurmCheckRunsInADryRun(t *testing.T) {
	rec := sinfoAnswers("exe0007 allocated\n", 0)
	h, err := run(t, harnessOptions{recorder: rec}, "bmc", "power", "off", "-n", "exe7", "--dry-run")
	if err == nil {
		t.Fatalf("the dry run approved powering off a busy node:\n%s", h.errOut)
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d (%v)", got, want, err)
	}
	if strings.Contains(h.errOut.String(), "Would power off") {
		t.Errorf("the dry run previewed the power-off:\n%s", h.errOut)
	}

	rec = sinfoAnswers("exe0007 idle\n", 0)
	h, err = run(t, harnessOptions{recorder: rec}, "bmc", "power", "off", "-n", "exe7", "--dry-run")
	if err != nil {
		t.Fatalf("the dry run of an idle node failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "Would power off") {
		t.Errorf("the dry run says nothing:\n%s", h.errOut)
	}
	if sinfo, other := sinfoCalls(rec); sinfo != 1 || other != 0 {
		t.Errorf("the dry run sent %d sinfo and %d other commands, want 1 and 0", sinfo, other)
	}
	// Nodes of hidden partitions run jobs too, so sinfo is asked for them.
	if got, want := rec.Commands()[0], "sinfo --all -h -N -o '%N %T' -n exe0007"; !strings.Contains(got, want) {
		t.Errorf("command = %q, want it to contain %q", got, want)
	}
}

// Section 3.5: a Redfish reset sent by hand powers a node off as surely as
// bmc power does.
func TestRedfishPostResetChecksSlurm(t *testing.T) {
	rec := sinfoAnswers("exe0007 allocated\n", 0)
	_, err := run(t, harnessOptions{recorder: rec}, "bmc", "redfish", "post",
		"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", `{"ResetType":"ForceOff"}`, "-n", "exe7", "-y")
	if err == nil {
		t.Fatal("a reset was posted to a node running a job")
	}
	if !strings.Contains(err.Error(), "--lose-jobs") {
		t.Errorf("error = %v, want it to name the override", err)
	}
	if sinfo, _ := sinfoCalls(rec); sinfo != 1 {
		t.Errorf("sinfo was sent %d times, want once", sinfo)
	}
}

// Section 3.6: a set whose host list is longer than one argument may be is
// still checked, instead of the oversized sinfo call switching the check off.
func TestSlurmCheckCoversLongHostLists(t *testing.T) {
	// Every other node of each rack: no two numbers are adjacent, so even the
	// folded host list is longer than the 128 KiB one argument may be.
	odd := make([]string, 0, 1000)
	for n := 1; n <= 1999; n += 2 {
		odd = append(odd, fmt.Sprintf("%04d", n))
	}
	expr := "r[01-30]n[" + strings.Join(odd, ",") + "]"
	var all strings.Builder
	for r := 1; r <= 30; r++ {
		for n := 1; n <= 2000; n++ {
			state := "idle"
			if r == 21 && n == 17 {
				state = "allocated"
			}
			fmt.Fprintf(&all, "r%02dn%04d %s\n", r, n, state)
		}
	}
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if isSinfo(req) {
			return &transport.Result{Target: tg, Stdout: all.String()}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	_, err := run(t, harnessOptions{recorder: rec}, "bmc", "power", "off", "-n", expr, "--dry-run")
	if err == nil {
		t.Fatal("the power action went ahead for a long set with one busy node")
	}
	if !strings.Contains(err.Error(), "r21n0017") {
		t.Errorf("error = %v, want it to name r21n0017", err)
	}
	if sinfo, _ := sinfoCalls(rec); sinfo != 1 {
		t.Errorf("sinfo was sent %d times, want once", sinfo)
	}
	// The list is too long for one argument, so sinfo is asked about every
	// node and the answer narrowed to the set.
	if got := rec.Commands()[0]; strings.Contains(got, " -n ") {
		t.Errorf("command carries the host list: %.200s", got)
	}
}
