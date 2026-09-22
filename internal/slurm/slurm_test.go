// SPDX-License-Identifier: LGPL-3.0-or-later

package slurm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func client(t *testing.T, out string) (*slurm.Client, *transport.Recorder) {
	t.Helper()
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: out}, nil
	}}
	return &slurm.Client{
		Runner: rec,
		Target: transport.Target{Name: "login", Host: "login.example.org", Role: "login"},
		Spec:   v1alpha1.SlurmSpec{Organization: "example", DefaultAccount: "default"},
	}, rec
}

func TestNodesParsesParsableOutput(t *testing.T) {
	t.Parallel()

	out := strings.Join([]string{
		"exe0001|idle|main|128|515000|amd,epyc|(null)||| ",
		"exe0002|drained|main|128|515000|amd,epyc|(null)|firmware update, ticket 4711|alice|2026-09-01T10:00:00",
		// A node in two partitions is listed twice and must be folded back.
		"exe0002|drained|debug|128|515000|amd,epyc|(null)|firmware update, ticket 4711|alice|2026-09-01T10:00:00",
	}, "\n")

	c, rec := client(t, out)
	nodes, err := c.Nodes(context.Background(), nodeset.MustParse("exe[1-2]"), nil)
	if err != nil {
		t.Fatalf("Nodes failed: %v", err)
	}
	if got, want := len(nodes), 2; got != want {
		t.Fatalf("got %d nodes, want %d", got, want)
	}
	if got, want := nodes[1].Reason, "firmware update, ticket 4711"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
	if got, want := nodes[1].BaseState(), "drained"; got != want {
		t.Errorf("BaseState = %q, want %q", got, want)
	}

	// The display format truncates and moves with the terminal width, so the
	// parsable form with an explicit field list is what gets asked for.
	command := rec.Commands()[0]
	if !strings.Contains(command, "--noheader") || !strings.Contains(command, "%N|%T") {
		t.Errorf("command = %q, want the explicit parsable format", command)
	}
	if !strings.Contains(command, "--nodes 'exe[1-2]'") {
		t.Errorf("command = %q, want the node list quoted as one argument", command)
	}
}

func TestBaseStateStripsFlags(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"IDLE*": "idle", "alloc~": "alloc", "drained#": "drained", "mixed": "mixed",
	} {
		if got := (slurm.Node{State: in}).BaseState(); got != want {
			t.Errorf("BaseState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDrainRequiresAReason(t *testing.T) {
	t.Parallel()

	c, rec := client(t, "")
	// A drained node with no reason is a node nobody dares resume.
	if err := c.Drain(context.Background(), nodeset.MustParse("exe1"), "  "); err == nil {
		t.Error("draining without a reason should be refused")
	}
	if len(rec.Calls()) != 0 {
		t.Error("nothing should have been sent")
	}

	if err := c.Drain(context.Background(), nodeset.MustParse("exe[1-2]"), "ticket 4711"); err != nil {
		t.Fatalf("Drain failed: %v", err)
	}
	command := rec.Commands()[0]
	for _, want := range []string{"scontrol update", "state=drain", "'reason=ticket 4711'"} {
		if !strings.Contains(command, want) {
			t.Errorf("command = %q, want it to contain %q", command, want)
		}
	}
}

func TestResume(t *testing.T) {
	t.Parallel()

	c, rec := client(t, "")
	if err := c.Resume(context.Background(), nodeset.MustParse("exe[1-2]")); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if !strings.Contains(rec.Commands()[0], "state=resume") {
		t.Errorf("command = %q", rec.Commands()[0])
	}
}

func TestJobsFilter(t *testing.T) {
	t.Parallel()

	out := "4711|alice|proj|main|RUNNING|exe[1-2]|2|256|1-00:00:00|10:00|1000|None|/home/alice|./run.sh"
	c, rec := client(t, out)
	jobs, err := c.Jobs(context.Background(), slurm.JobFilter{
		States: []string{"RUNNING"},
		Users:  []string{"alice"},
		Nodes:  nodeset.MustParse("exe[1-2]"),
	})
	if err != nil {
		t.Fatalf("Jobs failed: %v", err)
	}
	if got, want := len(jobs), 1; got != want {
		t.Fatalf("got %d jobs, want %d", got, want)
	}
	if got, want := jobs[0].Command, "./run.sh"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	if got, want := jobs[0].WorkDir, "/home/alice"; got != want {
		t.Errorf("workdir = %q, want %q", got, want)
	}
	command := rec.Commands()[0]
	for _, want := range []string{"--states RUNNING", "--user alice", "--nodelist 'exe[1-2]'"} {
		if !strings.Contains(command, want) {
			t.Errorf("command = %q, want %q", command, want)
		}
	}
}

func TestHistoryUsesATimeWindow(t *testing.T) {
	t.Parallel()

	c, rec := client(t, "4711|alice|proj|FAILED|1:0|exe1|00:05:00|s|e|sub|main")
	jobs, err := c.History(context.Background(), slurm.AccountingFilter{
		States: []string{"FAILED"},
		Since:  2 * time.Hour,
	})
	if err != nil {
		t.Fatalf("History failed: %v", err)
	}
	if got, want := jobs[0].ExitCode, "1:0"; got != want {
		t.Errorf("exit code = %q, want %q", got, want)
	}
	if !strings.Contains(rec.Commands()[0], "--starttime") {
		t.Errorf("command = %q, want a start time", rec.Commands()[0])
	}
}

func TestAccountsAndUsers(t *testing.T) {
	t.Parallel()

	c, _ := client(t, "proj|Project|example|alice,bob")
	accounts, err := c.Accounts(context.Background(), "")
	if err != nil {
		t.Fatalf("Accounts failed: %v", err)
	}
	if got, want := accounts[0].Coordinators, "alice,bob"; got != want {
		t.Errorf("coordinators = %q, want %q", got, want)
	}

	c, _ = client(t, "alice|proj|proj|1")
	users, err := c.Users(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Users failed: %v", err)
	}
	if got, want := users[0].DefaultAccount, "proj"; got != want {
		t.Errorf("default account = %q, want %q", got, want)
	}
}

func TestAddAccountFillsInTheDefaults(t *testing.T) {
	t.Parallel()

	c, rec := client(t, "")
	if err := c.AddAccount(context.Background(), "proj", "", ""); err != nil {
		t.Fatalf("AddAccount failed: %v", err)
	}
	command := rec.Commands()[0]
	for _, want := range []string{"organization=example", "description=proj", "--immediate"} {
		if !strings.Contains(command, want) {
			t.Errorf("command = %q, want %q", command, want)
		}
	}
}

func TestAddUserCreatesOrAssociates(t *testing.T) {
	t.Parallel()

	// A user the accounting database does not know yet is created.
	c, rec := client(t, "")
	if err := c.AddUser(context.Background(), "alice", "proj", ""); err != nil {
		t.Fatalf("AddUser failed: %v", err)
	}
	if !strings.Contains(rec.Commands()[1], "create user") {
		t.Errorf("command = %q, want a create", rec.Commands()[1])
	}

	// One it already knows gains another association instead.
	c, rec = client(t, "alice|other|other|1")
	if err := c.AddUser(context.Background(), "alice", "proj", ""); err != nil {
		t.Fatalf("AddUser failed: %v", err)
	}
	if !strings.Contains(rec.Commands()[1], "add user") {
		t.Errorf("command = %q, want an add", rec.Commands()[1])
	}
}

func TestHasPosixUserAsksGetent(t *testing.T) {
	t.Parallel()

	// getent answers from the directory, where id would also answer from the
	// local files and claim a user exists that the cluster does not know.
	c, rec := client(t, "alice:x:1000:1000::/home/alice:/bin/bash")
	ok, err := c.HasPosixUser(context.Background(), "alice")
	if err != nil || !ok {
		t.Fatalf("HasPosixUser = %v, %v", ok, err)
	}
	if !strings.Contains(rec.Commands()[0], "getent passwd alice") {
		t.Errorf("command = %q, want getent", rec.Commands()[0])
	}
}

func TestErrorsFromTheClientAreReported(t *testing.T) {
	t.Parallel()

	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, ExitCode: 1, Stderr: "sinfo: error: no such partition\n"}, nil
	}}
	c := &slurm.Client{Runner: rec, Target: transport.Target{Name: "login"}}
	if _, err := c.Partitions(context.Background(), "nope"); err == nil {
		t.Fatal("a failing client should be reported")
	} else if !strings.Contains(err.Error(), "no such partition") {
		t.Errorf("error = %v, want it to carry what the client said", err)
	}
}

func TestStateGroups(t *testing.T) {
	t.Parallel()

	groups := slurm.StateGroups()
	for _, name := range []string{"alloc", "idle", "drain", "down", "defect"} {
		if len(groups[name]) == 0 {
			t.Errorf("state group %q is empty", name)
		}
	}
}
