// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package slurm_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func client(t *testing.T, out string) (*slurm.Client, *transport.Recorder) {
	t.Helper()
	return replying(t, func(transport.Request) string { return out })
}

// rows answers sinfo and squeue with rows rendered for the format asked for.
func rows(t *testing.T, r ...slurm.Row) (*slurm.Client, *transport.Recorder) {
	t.Helper()
	return replying(t, func(req transport.Request) string { return slurm.Render(req, r...) })
}

// replying answers scontrol show config with a cluster name and every other
// client with what out returns.
func replying(t *testing.T, out func(transport.Request) string) (*slurm.Client, *transport.Recorder) {
	t.Helper()
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if strings.Join(req.Argv, " ") == "scontrol show config" {
			return &transport.Result{Target: tg, Stdout: "ClusterName = hpc\n"}, nil
		}
		return &transport.Result{Target: tg, Stdout: out(req)}, nil
	}}
	return &slurm.Client{
		Runner: rec,
		Target: transport.Target{Name: "login", Host: "login.example.org", Role: "login"},
		Spec:   v1alpha1.SlurmSpec{Organization: "example", DefaultAccount: "default"},
	}, rec
}

func TestNodesParsesParsableOutput(t *testing.T) {
	t.Parallel()

	c, rec := rows(t,
		slurm.Row{"N": "exe0001", "T": "idle", "R": "main", "c": "128", "f": "amd,epyc", "E": "none", "u": "Unknown", "H": "Unknown"},
		slurm.Row{"N": "exe0002", "T": "drained", "R": "main", "c": "128", "E": "firmware update, ticket 4711",
			"u": "alice", "H": "2026-09-01T10:00:00"},
		// A node in two partitions is listed twice and must be folded back.
		slurm.Row{"N": "exe0002", "T": "drained", "R": "debug", "c": "128", "E": "firmware update, ticket 4711",
			"u": "alice", "H": "2026-09-01T10:00:00"},
	)
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
	// sinfo prints "none" and a user for a node without a reason.
	if n := nodes[0]; n.Reason != "" || n.ReasonUser != "" || n.ReasonTime != "" {
		t.Errorf("a node without a reason has %q [%s] at %s", n.Reason, n.ReasonUser, n.ReasonTime)
	}

	// The display format truncates and moves with the terminal width, so the
	// parsable form with an explicit field list is what gets asked for.
	command := rec.Commands()[0]
	if !strings.Contains(command, "--noheader") || !strings.Contains(command, "%N|") {
		t.Errorf("command = %q, want the explicit parsable format", command)
	}
	if !strings.Contains(command, "--nodes 'exe[1-2]'") {
		t.Errorf("command = %q, want the node list quoted as one argument", command)
	}
}

func TestNodesCannotBeForgedThroughAReason(t *testing.T) {
	t.Parallel()

	// Anyone who can drain a node writes its reason, and sinfo prints it as
	// it is, line breaks and separators included.
	forged := "failing DIMM\nexe[0001-0064]|drained|main|128|0||||x|y|\nwlm01|drained|main"
	c, _ := rows(t,
		slurm.Row{"N": "exe0001", "T": "idle", "E": "none"},
		slurm.Row{"N": "exe0007", "T": "drained", "E": forged},
	)
	nodes, err := c.Nodes(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Nodes failed: %v", err)
	}
	if got, want := len(nodes), 2; got != want {
		t.Fatalf("got %d nodes, want %d: %+v", got, want, nodes)
	}
	if got := nodes[1].Reason; got != forged {
		t.Errorf("reason = %q, want it whole", got)
	}
}

func TestNodesRefusesOutputOfTheWrongShape(t *testing.T) {
	t.Parallel()

	for name, out := range map[string]string{
		"a plain line":        "exe0001|idle|main\n",
		"a line from nowhere": "sinfo: warning: something\n",
	} {
		c, _ := client(t, out)
		if _, err := c.Nodes(context.Background(), nil, nil); err == nil {
			t.Errorf("%s: Nodes accepted %q", name, out)
		}
	}
}

func TestNodesKeepsOnlyTheStateAskedFor(t *testing.T) {
	t.Parallel()

	// sinfo --states idle matches the base state, so it returns drained
	// nodes as well.
	c, _ := rows(t,
		slurm.Row{"N": "exe0001", "T": "idle"},
		slurm.Row{"N": "exe0002", "T": "idle~"},
		slurm.Row{"N": "exe0007", "T": "drained"},
		slurm.Row{"N": "exe0008", "T": "draining"},
	)
	for group, want := range map[string]string{
		"idle":  "exe[0001-0002]",
		"drain": "exe[0007-0008]",
		// A flag cannot be read back from the reported state; sinfo decides.
		"idle,power_down": "exe[0001-0002,0007-0008]",
	} {
		ns, err := c.NodeSet(context.Background(), slurm.States(group))
		if err != nil {
			t.Fatalf("NodeSet failed: %v", err)
		}
		if got := ns.String(); got != want {
			t.Errorf("NodeSet(%s) = %q, want %q", group, got, want)
		}
	}
}

func TestCheckNodesRefusesWhatSlurmExpands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes string
		sinfo []string
		want  string
	}{
		{"exactly the set", "exe[1-2]", []string{"exe1", "exe2"}, ""},
		{"ALL", "ALL", nil, "every node"},
		{"all among others", "exe1,All", nil, "every node"},
		{"a NodeSet name", "gpunodes", []string{"exe9", "exe10"}, "more nodes than were named"},
		{"a node Slurm does not know", "exe[1-2],zz1", []string{"exe1", "exe2"}, "does not know zz1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, rec := client(t, strings.Join(tc.sinfo, "\n"))
			err := c.CheckNodes(context.Background(), nodeset.MustParse(tc.nodes))
			if tc.want == "" {
				if err != nil {
					t.Errorf("CheckNodes = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("CheckNodes = %v, want %q", err, tc.want)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}
			for _, call := range rec.Calls() {
				if call.Request.Argv[0] != "sinfo" {
					t.Errorf("sent %q", call.Command)
				}
			}
		})
	}
}

func TestValidateReason(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"ticket 4711: failing DIMM", "rack R02 – Wartung", strings.Repeat("x", slurm.MaxReasonLength)} {
		if err := slurm.ValidateReason(ok); err != nil {
			t.Errorf("ValidateReason(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "none", "(null)", "a\nb", "a\rb", "a\tb", "a\x1b[2Kb", "a\u009bb", "a\x7fb",
		"a|b", "\xff", strings.Repeat("x", slurm.MaxReasonLength+1)} {
		err := slurm.ValidateReason(bad)
		if err == nil {
			t.Errorf("ValidateReason(%q) accepted it", bad)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("ValidateReason(%q): exit code = %d, want %d", bad, got, want)
		}
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
	// ALL is every node to slurmctld.
	if err := c.Drain(context.Background(), nodeset.MustParse("all"), "maint"); err == nil {
		t.Error("draining ALL should be refused")
	}
	if err := c.Resume(context.Background(), nodeset.MustParse("ALL")); err == nil {
		t.Error("resuming ALL should be refused")
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

	// Any user writes the working directory and the command line.
	command := "./run.sh --sep '|'\n4712|mallory|proj|main|RUNNING|wlm01"
	c, rec := rows(t, slurm.Row{"i": "4711", "u": "alice", "a": "proj", "P": "main", "T": "RUNNING",
		"N": "exe[1-2]", "D": "2", "C": "256", "l": "1-00:00:00", "M": "10:00", "Q": "1000", "r": "None",
		"Z": "/home/alice|x", "o": command})
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
	if got, want := jobs[0].Command, command; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	if got, want := jobs[0].WorkDir, "/home/alice|x"; got != want {
		t.Errorf("workdir = %q, want %q", got, want)
	}
	sent := rec.Commands()[0]
	for _, want := range []string{"--states RUNNING", "--user alice", "--nodelist 'exe[1-2]'"} {
		if !strings.Contains(sent, want) {
			t.Errorf("command = %q, want %q", sent, want)
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
	// A time of day would be read in the zone of the login node.
	if !strings.Contains(rec.Commands()[0], "--starttime now-7200 ") {
		t.Errorf("command = %q, want the start relative to now", rec.Commands()[0])
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
	command := rec.Commands()[1]
	for _, want := range []string{"cluster=hpc", "organization=example", "description=proj", "--immediate"} {
		if !strings.Contains(command, want) {
			t.Errorf("command = %q, want %q", command, want)
		}
	}
}

func TestAddUserCreatesOrAssociates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing string
		account  string
		def      string
		preview  string
		sent     []string
	}{
		{"a new user", "", "proj", "",
			"create the Slurm user alice with the account proj in the Slurm cluster hpc",
			[]string{"add user names=alice account=proj defaultaccount=proj cluster=hpc"}},
		{"a new user with another default", "", "", "other",
			"create the Slurm user alice with the accounts default and other, other the default in the Slurm cluster hpc",
			[]string{"add user names=alice account=default,other defaultaccount=other cluster=hpc"}},
		{"a known user", "alice|other|other\n", "proj", "",
			"associate alice with the account proj in the Slurm cluster hpc",
			[]string{"add user names=alice account=proj cluster=hpc"}},
		{"a known user and a new default", "alice|other|other\n", "proj", "proj",
			"associate alice with the account proj and make proj the default account of alice instead of other in the Slurm cluster hpc",
			[]string{"add user names=alice account=proj cluster=hpc",
				"modify user where name=alice cluster=hpc set defaultaccount=proj"}},
		{"a known user and the default unchanged", "alice|proj|proj\nalice|other|proj\n", "other", "proj",
			"leave alice associated with other in the Slurm cluster hpc", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, rec := client(t, tc.existing)
			u, err := c.PlanUserAdd(context.Background(), "alice", tc.account, tc.def)
			if err != nil {
				t.Fatalf("PlanUserAdd failed: %v", err)
			}
			if got := u.String(); got != tc.preview {
				t.Errorf("preview = %q\nwant      %q", got, tc.preview)
			}
			if got, want := u.Changes(), len(tc.sent) > 0; got != want {
				t.Errorf("Changes = %v, want %v", got, want)
			}
			reads := len(rec.Calls())
			if err := c.AddUser(context.Background(), u); err != nil {
				t.Fatalf("AddUser failed: %v", err)
			}
			var sent []string
			for _, call := range rec.Calls()[reads:] {
				sent = append(sent, strings.TrimPrefix(strings.Join(call.Request.Argv, " "), "sacctmgr --immediate "))
			}
			if strings.Join(sent, "\n") != strings.Join(tc.sent, "\n") {
				t.Errorf("sent %q, want %q", sent, tc.sent)
			}
		})
	}
}

func TestAccountingRefusesNamesSacctmgrExpands(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"proj[1-100]", "a,b", "", "a b", "-x", "a=b"} {
		if err := slurm.ValidateName("account", bad); err == nil {
			t.Errorf("ValidateName(%q) accepted it", bad)
		}
	}
	if err := slurm.ValidateUserName("1000"); err == nil {
		t.Error("a user ID was accepted as a user name")
	}
	for _, ok := range []string{"proj", "phy.lab-2", "alice@EXAMPLE"} {
		if err := slurm.ValidateName("account", ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
}

func TestHasPosixUserAsksGetent(t *testing.T) {
	t.Parallel()

	c, rec := client(t, "alice:x:1000:1000::/home/alice:/bin/bash")
	ok, err := c.HasPosixUser(context.Background(), "alice")
	if err != nil || !ok {
		t.Fatalf("HasPosixUser = %v, %v", ok, err)
	}
	if !strings.Contains(rec.Commands()[0], "getent passwd alice") {
		t.Errorf("command = %q, want getent", rec.Commands()[0])
	}

	// getent exits 2 for a name it does not know, which over ssh arrives
	// as an error of the transport.
	rec = &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, ExitCode: 2, Err: errors.New("login: command exited 2")}, nil
	}}
	c = &slurm.Client{Runner: rec, Target: transport.Target{Name: "login"}}
	if ok, err := c.HasPosixUser(context.Background(), "ghost"); ok || err != nil {
		t.Errorf("HasPosixUser(ghost) = %v, %v; want false and no error", ok, err)
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

func TestErrorsKeepSlurmsMessageAndTheTransportsCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result transport.Result
		code   int
		want   string
	}{
		{"a refusal over ssh", transport.Result{ExitCode: 1, Stderr: "slurm_update error: Invalid node name specified\n",
			Err: errors.New("login: command exited 1")}, exitcode.TargetFailed, "scontrol on login exited 1: slurm_update error: Invalid node name specified"},
		{"a refusal with no message", transport.Result{ExitCode: 1}, exitcode.TargetFailed, "scontrol on login exited 1"},
		{"control characters in the message", transport.Result{ExitCode: 1, Stderr: "a\x1b[2Kb\nsecond line\n"},
			exitcode.TargetFailed, `exited 1: a\x1b[2Kb; second line`},
		{"bidirectional controls and bytes that are not UTF-8", transport.Result{ExitCode: 1, Stderr: "a\u202eb\u2028c\xff\n"},
			exitcode.TargetFailed, `exited 1: a\u202eb\u2028c\xff`},
		{"an unreachable login node", transport.Result{ExitCode: 255,
			Err: exitcode.Wrap(exitcode.Transport, errors.New("login: Connection refused"))}, exitcode.Transport, "Connection refused"},
		{"ssh could not run", transport.Result{ExitCode: -1, Err: errors.New("running ssh for login: not found")},
			exitcode.Transport, "not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
				result := tc.result
				result.Target = tg
				return &result, nil
			}}
			c := &slurm.Client{Runner: rec, Target: transport.Target{Name: "login", Host: "login"}}
			err := c.Resume(context.Background(), nodeset.MustParse("exe1"))
			if got := exitcode.From(err); got != tc.code {
				t.Errorf("exit code = %d, want %d (%v)", got, tc.code, err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAFailedUpdateSaysWhichNodesChanged(t *testing.T) {
	t.Parallel()

	var updated bool
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if req.Argv[0] == "scontrol" {
			updated = true
			return &transport.Result{Target: tg, ExitCode: 1, Stderr: "slurm_update error: Invalid node state specified\n"}, nil
		}
		// slurmctld drained the nodes before the one it stopped at.
		reason := "none"
		if updated {
			reason = "ticket 42"
		}
		return &transport.Result{Target: tg, Stdout: slurm.Render(req,
			slurm.Row{"N": "exe1", "T": "drained", "E": reason},
			slurm.Row{"N": "exe2", "T": "drained", "E": "older reason"},
			slurm.Row{"N": "exe3", "T": "idle", "E": "none"},
		)}, nil
	}}
	c := &slurm.Client{Runner: rec, Target: transport.Target{Name: "login"}}
	err := c.Drain(context.Background(), nodeset.MustParse("exe[1-3]"), "ticket 42")
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "afterwards exe1 is drained with this reason, exe[2-3] are not") {
		t.Errorf("error = %v", err)
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

// The summary of the command line and that of the MCP server broke ties
// differently, the command line on the user alone and in random order.
func TestCountJobsOrdersEqualCountsByUserAccountAndPartition(t *testing.T) {
	t.Parallel()

	jobs := []slurm.Job{
		{User: "bob", Account: "a", Partition: "bc"},
		{User: "bob", Account: "ab", Partition: "c"},
		{User: "alice", Account: "x", Partition: "main"},
		{User: "carol", Account: "x", Partition: "main"},
		{User: "carol", Account: "x", Partition: "main"},
	}
	var got []string
	for _, c := range slurm.CountJobs(jobs) {
		got = append(got, fmt.Sprintf("%s/%s/%s=%d", c.User, c.Account, c.Partition, c.Jobs))
	}
	want := []string{"carol/x/main=2", "alice/x/main=1", "bob/a/bc=1", "bob/ab/c=1"}
	if !slices.Equal(got, want) {
		t.Errorf("CountJobs = %q, want %q", got, want)
	}
}
