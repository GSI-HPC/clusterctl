// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The Slurm commands run sinfo, squeue and scontrol on the login role, the
// submitter, and parse what they print. These tests hand them the output of
// a real slurmctld, of each Slurm release line CI runs, and check each
// change they make with scontrol on the controller, past clusterctl.

// waitIdle waits until every worker is idle, which a test that looks at
// node states needs after one that drained a node or ran a job.
func waitIdle(t *testing.T) {
	t.Helper()
	if err := waitForSlurm(time.Minute); err != nil {
		t.Fatal(err)
	}
}

// slurmNode is a node as slurm node list -o json prints it.
type slurmNode struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Partition string `json:"partition"`
	Reason    string `json:"reason"`
}

// scontrolNode returns what slurmctld says of a node.
func scontrolNode(t *testing.T, node string) string {
	t.Helper()
	return onNode(t, "controller", "scontrol", "show", "node", node)
}

func TestSlurmNodeList(t *testing.T) {
	waitIdle(t)
	r := clusterctl(t, "slurm", "node", "list", "-o", "json")
	r.wantCode(t, 0)
	nodes := decode[[]slurmNode](t, r)
	var names []string
	for _, n := range nodes {
		names = append(names, n.Name)
		if n.State != "idle" || n.Partition != "all" || n.Reason != "" {
			t.Errorf("%s is %q in %q with the reason %q, want idle in all with none", n.Name, n.State, n.Partition, n.Reason)
		}
	}
	if !slices.Equal(names, workers) {
		t.Errorf("Slurm lists %v, want %v", names, workers)
	}

	r = clusterctl(t, "slurm", "node", "nodeset", "idle")
	r.wantCode(t, 0)
	if r.stdout != "worker-[0-2]\n" {
		t.Errorf("the idle nodes are %q, want worker-[0-2]", r.stdout)
	}
}

func TestSlurmPartition(t *testing.T) {
	r := clusterctl(t, "slurm", "partition", "-o", "json")
	r.wantCode(t, 0)
	partitions := decode[[]struct {
		Name      string `json:"name"`
		Available string `json:"available"`
		Nodes     string `json:"nodes"`
		NodeCount string `json:"nodeCount"`
	}](t, r)
	if len(partitions) != 1 {
		t.Fatalf("got %d partitions, want the one sind makes: %s", len(partitions), r)
	}
	p := partitions[0]
	if p.Name != "all" || p.Available != "up" || p.Nodes != "worker-[0-2]" || p.NodeCount != "3" {
		t.Errorf("got %+v, want all, up, with the 3 nodes worker-[0-2]", p)
	}
}

// TestSlurmGroupResolvesThroughSinfo resolves a group of the exec source,
// which runs sinfo on the login role, the way ClusterShell's group sources
// ask the workload manager.
func TestSlurmGroupResolvesThroughSinfo(t *testing.T) {
	r := clusterctl(t, "node", "select", "@slurm:all")
	r.wantCode(t, 0)
	if r.stdout != "worker-[0-2]\n" {
		t.Errorf("@slurm:all is %q, want worker-[0-2]", r.stdout)
	}
}

func TestSlurmDrainAndResume(t *testing.T) {
	waitIdle(t)
	const reason = "clusterctl e2e: drain and resume"
	t.Cleanup(func() {
		// It fails when the node is already back, which is what the test
		// leaves when it passes.
		_, _ = dockerExec("controller", "scontrol", "update", "nodename=worker-2", "state=resume")
	})

	r := clusterctl(t, "slurm", "node", "drain", "-y", reason, "-n", "worker-2")
	r.wantCode(t, 0)
	if !strings.Contains(r.stderr, "drained worker-2\n") {
		t.Errorf("drain does not say what it did: %s", r)
	}
	if got := scontrolNode(t, "worker-2"); !strings.Contains(got, "+DRAIN") || !strings.Contains(got, "Reason="+reason) {
		t.Errorf("slurmctld does not have worker-2 drained with the reason given:\n%s", got)
	}

	r = clusterctl(t, "slurm", "node", "list", "--state", "drain", "-o", "json")
	r.wantCode(t, 0)
	drained := decode[[]slurmNode](t, r)
	if len(drained) != 1 || drained[0].Name != "worker-2" || drained[0].Reason != reason {
		t.Errorf("the drained nodes are %+v, want worker-2 with the reason %q", drained, reason)
	}

	r = clusterctl(t, "slurm", "node", "resume", "-y", "-n", "worker-2")
	r.wantCode(t, 0)
	if !strings.Contains(r.stderr, "resumed worker-2\n") {
		t.Errorf("resume does not say what it did: %s", r)
	}
	if got := scontrolNode(t, "worker-2"); strings.Contains(got, "DRAIN") {
		t.Errorf("slurmctld still has worker-2 drained:\n%s", got)
	}
}

// TestSlurmDrainChangesNothingItWasNotAllowedTo checks two refusals made
// before scontrol runs: a change nobody confirmed, and a node set holding a
// node slurmctld does not know, which scontrol would stop at after changing
// the nodes before it.
func TestSlurmDrainChangesNothingItWasNotAllowedTo(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // in the error
	}{
		{
			name: "no confirmation and no terminal to ask on",
			args: []string{"-n", "worker-1"},
			want: "no terminal to ask on",
		},
		{
			name: "a node Slurm does not know",
			args: []string{"-y", "-n", "worker-[1,9]"},
			want: "Slurm does not know worker-9",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			waitIdle(t)
			t.Cleanup(func() {
				_, _ = dockerExec("controller", "scontrol", "update", "nodename=worker-1", "state=resume")
			})
			r := clusterctl(t, append([]string{"slurm", "node", "drain", "clusterctl e2e: must not happen"}, tt.args...)...)
			r.wantCode(t, 2)
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("the refusal does not say %q: %s", tt.want, r)
			}
			if got := scontrolNode(t, "worker-1"); strings.Contains(got, "DRAIN") {
				t.Errorf("worker-1 was drained all the same:\n%s", got)
			}
		})
	}
}

// TestSlurmJobList submits a job past clusterctl, on the submitter, and
// finds it with the filters of job list. It comes last, since the node the
// job ran on is completing for a moment after it was cancelled.
func TestSlurmJobList(t *testing.T) {
	waitIdle(t)
	out := onNode(t, "submitter", "sbatch", "--parsable", "--job-name=clusterctl-e2e", "--nodelist=worker-1",
		"--output=/dev/null", "--wrap", "sleep 600")
	id, _, _ := strings.Cut(strings.TrimSpace(out), ";")
	t.Cleanup(func() {
		_, _ = dockerExec("submitter", "scancel", id)
		// The next test may look at node states, so the job has to be
		// gone rather than completing.
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			if out, err := dockerExec("submitter", "squeue", "--noheader", "--jobs", id); err != nil || strings.TrimSpace(out) == "" {
				return
			}
			time.Sleep(time.Second)
		}
	})
	waitRunning(t, id)

	type job struct {
		ID        string `json:"id"`
		User      string `json:"user"`
		Partition string `json:"partition"`
		State     string `json:"state"`
		Nodes     string `json:"nodes"`
	}
	find := func(args ...string) (job, bool) {
		t.Helper()
		r := clusterctl(t, append([]string{"slurm", "job", "list", "-o", "json"}, args...)...)
		r.wantCode(t, 0)
		for _, j := range decode[[]job](t, r) {
			if j.ID == id {
				return j, true
			}
		}
		return job{}, false
	}

	j, ok := find("--state", "running")
	if !ok {
		t.Fatalf("job %s is not among the running jobs", id)
	}
	if j.User != "root" || j.Partition != "all" || j.State != "RUNNING" || j.Nodes != "worker-1" {
		t.Errorf("job %s is %+v, want root's, running in all on worker-1", id, j)
	}
	if _, ok := find("-n", "worker-1"); !ok {
		t.Errorf("job %s is not among the jobs of worker-1", id)
	}
	if _, ok := find("-n", "worker-0"); ok {
		t.Errorf("job %s is among the jobs of worker-0, where it does not run", id)
	}
	if _, ok := find("--state", "pending"); ok {
		t.Errorf("job %s is among the pending jobs", id)
	}
}

// waitRunning waits until slurmctld has started a job.
func waitRunning(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := dockerExec("submitter", "squeue", "--noheader", "--jobs", id, "--format", "%T")
		if err == nil && strings.TrimSpace(out) == "RUNNING" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not start within a minute: %v %s", id, err, out)
		}
		time.Sleep(time.Second)
	}
}
