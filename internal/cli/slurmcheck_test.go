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
