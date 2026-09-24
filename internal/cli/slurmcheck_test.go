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
