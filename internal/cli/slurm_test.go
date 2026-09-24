// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// slurmCluster fakes the Slurm clients on the login node closely enough
// that the checks made before a change have something to find: slurmctld
// expands ALL and NodeSet names, sinfo matches a state against the base
// state and the flags, and scontrol update is not atomic.
type slurmCluster struct {
	// nodes are the nodes Slurm knows, in the order sinfo lists them. The
	// key "match" lists the state names sinfo selects a node by.
	nodes []slurm.Row
	// nodeSets are the NodeSet definitions of slurm.conf.
	nodeSets map[string]string
	// users are the accounts getent knows.
	users map[string]bool
	// associations is what sacctmgr show user answers, one
	// user|account|default|fairshare line each.
	associations string
	// update is the result of scontrol update; after it, applied names the
	// nodes the update changed anyway.
	update  *transport.Result
	applied func(*slurmCluster)

	mu   sync.Mutex
	sent []string
}

func newSlurmCluster() *slurmCluster {
	return &slurmCluster{
		nodes: []slurm.Row{
			{"N": "exe0001", "T": "idle", "R": "main", "c": "128", "E": "none", "u": "Unknown", "match": "idle"},
			{"N": "exe0002", "T": "idle", "R": "main", "c": "128", "E": "none", "u": "root", "match": "idle"},
			// A node that stopped answering while running a job.
			{"N": "exe0003", "T": "mixed*", "R": "main", "c": "128", "E": "Not responding", "u": "slurm",
				"match": "mix mixed no_respond"},
			// A node queued for power saving.
			{"N": "exe0004", "T": "idle!", "R": "main", "c": "128", "E": "none", "u": "Unknown",
				"match": "idle power_down"},
			{"N": "exe0007", "T": "drained", "R": "main", "c": "128", "E": "failing DIMM, do not touch", "u": "alice",
				"match": "idle drain drained"},
			{"N": "exe0009", "T": "idle", "R": "gpu", "c": "64", "E": "none", "u": "Unknown", "match": "idle"},
			{"N": "exe0010", "T": "idle", "R": "gpu", "c": "64", "E": "none", "u": "Unknown", "match": "idle"},
			{"N": "wlm01", "T": "idle", "R": "admin", "c": "16", "E": "none", "u": "Unknown", "match": "idle"},
		},
		nodeSets: map[string]string{"gpunodes": "exe[0009-0010]"},
		users:    map[string]bool{"alice": true},
	}
}

func (c *slurmCluster) record(argv []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, strings.Join(argv, " "))
}

// changes returns what was sent that changes the cluster.
func (c *slurmCluster) changes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.sent {
		if strings.HasPrefix(s, "scontrol update") || strings.HasPrefix(s, "sacctmgr --immediate") {
			out = append(out, s)
		}
	}
	return out
}

// sentBy returns the command lines of one client.
func (c *slurmCluster) sentBy(program string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.sent {
		if strings.HasPrefix(s, program+" ") {
			out = append(out, s)
		}
	}
	return out
}

func (c *slurmCluster) recorder() *transport.Recorder {
	return &transport.Recorder{Reply: c.reply}
}

func (c *slurmCluster) reply(target transport.Target, req transport.Request) (*transport.Result, error) {
	result := &transport.Result{Target: target}
	c.record(req.Argv)
	switch req.Argv[0] {
	case "sinfo":
		result.Stdout = slurm.Render(req, c.selected(req)...)
	case "squeue":
		result.Stdout = ""
	case "scontrol":
		if len(req.Argv) > 1 && req.Argv[1] == "show" {
			result.Stdout = "Configuration data as of 2026-09-24T10:00:00\nClusterName             = hpc\nControlMachine          = wlm01\n"
			break
		}
		if c.update != nil {
			result = c.update
			result.Target = target
		}
		if c.applied != nil {
			c.applied(c)
		}
	case "getent":
		name := req.Argv[len(req.Argv)-1]
		if !c.users[name] {
			result.ExitCode = 2
			// Over ssh the transport reports every non-zero exit as an error.
			result.Err = errors.New(target.String() + ": command exited 2")
			break
		}
		result.Stdout = name + ":x:1000:1000:" + name + ":/home/" + name + ":/bin/bash\n"
	case "sacctmgr":
		if len(req.Argv) > 3 && req.Argv[3] == "show" {
			result.Stdout = c.associations
		}
	}
	return result, nil
}

// selected returns the nodes a sinfo call selects, reading --nodes the way
// slurmctld does.
func (c *slurmCluster) selected(req transport.Request) []slurm.Row {
	var wanted *nodeset.NodeSet
	if list := slurm.Arg(req, "--nodes"); list != "" {
		wanted = nodeset.New()
		for _, word := range strings.Split(list, ",") {
			switch {
			case strings.EqualFold(word, "all"):
				for _, n := range c.nodes {
					_ = wanted.Add(n["N"])
				}
			case c.nodeSets[word] != "":
				_ = wanted.Add(c.nodeSets[word])
			default:
				_ = wanted.Add(word)
			}
		}
	}
	var states []string
	if s := slurm.Arg(req, "--states"); s != "" {
		states = strings.Split(s, ",")
	}
	var out []slurm.Row
	for _, n := range c.nodes {
		if wanted != nil && !wanted.Contains(n["N"]) {
			continue
		}
		if len(states) > 0 && !matchesAny(n["match"], states) {
			continue
		}
		out = append(out, n)
	}
	return out
}

func matchesAny(match string, states []string) bool {
	for _, have := range strings.Fields(match) {
		for _, want := range states {
			if have == want {
				return true
			}
		}
	}
	return false
}

func (c *slurmCluster) node(name string) slurm.Row {
	for _, n := range c.nodes {
		if n["N"] == name {
			return n
		}
	}
	return nil
}

// 3.4: slurmctld reads ALL as every node and a NodeSet name as its members,
// while clusterctl previewed each as one host nobody knew.
func TestSlurmChangesRefuseNamesSlurmExpands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"resume ALL", []string{"slurm", "node", "resume", "-n", "ALL", "-y"}, "every node"},
		{"resume all in lower case", []string{"slurm", "node", "resume", "-n", "exe0001,all", "-y"}, "every node"},
		{"drain a NodeSet name", []string{"slurm", "node", "drain", "maint", "-n", "gpunodes", "-y"}, "exe[0009-0010]"},
		{"drain a node Slurm does not know", []string{"slurm", "node", "drain", "maint", "-n", "exe[0001-0002],zz1", "-y"}, "zz1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := newSlurmCluster()
			h, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: cluster.recorder()}, tc.args...)
			if err == nil {
				t.Fatalf("the change went ahead:\n%s%s", h.out, h.errOut)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d (%v)", got, want, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if changes := cluster.changes(); len(changes) != 0 {
				t.Errorf("sent %v", changes)
			}
			if strings.Contains(h.errOut.String(), "About to") {
				t.Errorf("the set was previewed before it was checked:\n%s", h.errOut)
			}
		})
	}
}

// 3.4, 2.13: the check reads the cluster for real in a dry run, which still
// changes nothing.
func TestSlurmDrainDryRunChecksTheSetAndSendsNothing(t *testing.T) {
	cluster := newSlurmCluster()
	h, err := run(t, harnessOptions{recorder: cluster.recorder()},
		"slurm", "node", "drain", "ticket 4711: failing DIMM", "-n", "exe[1-2]", "--dry-run")
	if err != nil {
		t.Fatalf("the dry run failed: %v", err)
	}
	if len(cluster.sentBy("sinfo")) == 0 {
		t.Error("the set was not checked against Slurm")
	}
	if changes := cluster.changes(); len(changes) != 0 {
		t.Errorf("a dry run sent %v", changes)
	}
	if !strings.Contains(h.errOut.String(), "Would drain 2 hosts") {
		t.Errorf("the dry run says:\n%s", h.errOut)
	}

	_, err = run(t, harnessOptions{recorder: newSlurmCluster().recorder()},
		"slurm", "node", "drain", "maint", "-n", "ALL", "--dry-run")
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("a dry run of ALL: exit code = %d, want %d", got, want)
	}
}

// 10.4, 12.1, 2.14: the reason is checked before anything is shown or sent.
func TestSlurmDrainRefusesBadReasonsBeforeTheGate(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{"blank", "   ", "needs a reason"},
		{"a newline", "ticket 42\nAbout to drain 1 host: exe0001", "control character"},
		{"an escape sequence", "ticket 42\x1b[1A\x1b[2K\r", "control character"},
		{"a C1 control", "ticket 42\u009b2K", "control character"},
		{"the separator", "ticket 42|exe0002|idle", "|"},
		{"what sinfo prints for none", "none", "no reason"},
		{"too long", strings.Repeat("x", 300), "longer than"},
	}
	for _, tc := range tests {
		for _, dry := range []bool{false, true} {
			name := tc.name
			args := []string{"slurm", "node", "drain", tc.reason, "-n", "exe0001", "-y"}
			if dry {
				name += " dry run"
				args = append(args, "--dry-run")
			}
			t.Run(name, func(t *testing.T) {
				cluster := newSlurmCluster()
				h, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: cluster.recorder()}, args...)
				if got, want := exitcode.From(err), exitcode.Usage; got != want {
					t.Fatalf("exit code = %d, want %d (%v)", got, want, err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error = %v, want it to say %q", err, tc.want)
				}
				if strings.Contains(h.errOut.String(), "drain") {
					t.Errorf("the drain was previewed:\n%s", h.errOut)
				}
				if changes := cluster.changes(); len(changes) != 0 {
					t.Errorf("sent %v", changes)
				}
			})
		}
	}
}

// 10.4: the reason is quoted in the preview.
func TestSlurmDrainQuotesTheReason(t *testing.T) {
	cluster := newSlurmCluster()
	h, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: cluster.recorder()},
		"slurm", "node", "drain", `ticket 4711: "failing" DIMM`, "-n", "exe0001")
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), `reason: "ticket 4711: \"failing\" DIMM"`) {
		t.Errorf("the preview does not quote the reason:\n%s", h.errOut)
	}
	if changes := cluster.changes(); len(changes) != 1 ||
		!strings.Contains(changes[0], `reason=ticket 4711: "failing" DIMM`) {
		t.Errorf("sent %v", changes)
	}
}

// 2.15: a forgotten reason must not turn the node names into the reason,
// and nodes named twice must not silently drop one of the two.
func TestSlurmDrainRefusesAReasonThatNamesNodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"a node as the reason", []string{"exe0007", "-n", "exe0001"}, "names nodes"},
		{"a node set as the reason", []string{"exe[1-2]", "-n", "exe0001"}, "names nodes"},
		{"a group as the reason", []string{"@rack:R02", "-n", "exe0001"}, "names nodes"},
		{"nodes after the reason and -n", []string{"ticket", "4711", "DIMM", "-n", "exe0007"}, "both as an argument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster := newSlurmCluster()
			args := append([]string{"slurm", "node", "drain"}, tc.args...)
			_, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: cluster.recorder()}, append(args, "-y")...)
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Fatalf("exit code = %d, want %d (%v)", got, want, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to say %q", err, tc.want)
			}
			if changes := cluster.changes(); len(changes) != 0 {
				t.Errorf("sent %v", changes)
			}
		})
	}
}

// 4.11: a state group means the state Slurm reports, not every node whose
// base state or flags sinfo happens to match.
func TestSlurmStateGroupsMatchTheReportedState(t *testing.T) {
	tests := []struct {
		group string
		want  string
	}{
		// exe0007 is drained: its base state is idle, but it is not idle.
		{"idle", "exe[0001-0002,0004,0009-0010],wlm01"},
		// exe0003 is not responding and exe0004 is queued for power saving;
		// neither is down.
		{"down", ""},
		{"drain", "exe0007"},
		{"alloc", "exe0003"},
	}
	for _, tc := range tests {
		t.Run(tc.group, func(t *testing.T) {
			h, err := run(t, harnessOptions{recorder: newSlurmCluster().recorder()}, "slurm", "node", "nodeset", tc.group)
			if err != nil {
				t.Fatalf("nodeset failed: %v", err)
			}
			if got := strings.TrimSpace(h.out.String()); got != tc.want {
				t.Errorf("nodeset %s = %q, want %q", tc.group, got, tc.want)
			}
		})
	}
}

// 12.1: a drain reason with a line break must not become rows of its own.
func TestSlurmOutputCannotBeForgedThroughAReason(t *testing.T) {
	cluster := newSlurmCluster()
	cluster.nodes[4]["E"] = "failing DIMM\nexe[0001-0064]|drained|main|128|0||||x|y\nwlm01|drained|main|128|0||||x|y"

	h, err := run(t, harnessOptions{recorder: cluster.recorder()}, "slurm", "node", "nodeset", "drain")
	if err != nil {
		t.Fatalf("nodeset failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "exe0007"; got != want {
		t.Errorf("nodeset drain = %q, want %q", got, want)
	}

	h, err = run(t, harnessOptions{recorder: cluster.recorder()}, "slurm", "node", "list", "-o", "json")
	if err != nil {
		t.Fatalf("node list failed: %v", err)
	}
	if strings.Count(h.out.String(), `"name"`) != len(cluster.nodes) {
		t.Errorf("node list has rows Slurm did not send:\n%s", h.out)
	}
	if !strings.Contains(h.out.String(), `"reason": "failing DIMM\nexe[0001-0064]|drained`) {
		t.Errorf("the reason was not kept whole:\n%s", h.out)
	}
}

// 12.6: sinfo prints "none" for no reason, and a reason user with it.
func TestSlurmNodeListShowsNoReasonForHealthyNodes(t *testing.T) {
	h, err := run(t, harnessOptions{recorder: newSlurmCluster().recorder()}, "slurm", "node", "list")
	if err != nil {
		t.Fatalf("node list failed: %v", err)
	}
	out := h.out.String()
	for _, bad := range []string{"none", "Unknown", "[root]"} {
		if strings.Contains(out, bad) {
			t.Errorf("the list shows %q for a node without a reason:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "failing DIMM, do not touch [alice]") {
		t.Errorf("the real reason is missing:\n%s", out)
	}
}

// 11.6: Slurm's own message reaches the administrator, and exit codes say
// who failed.
func TestSlurmFailuresKeepSlurmsMessageAndTheRightCode(t *testing.T) {
	t.Run("a refused update", func(t *testing.T) {
		cluster := newSlurmCluster()
		cluster.update = &transport.Result{ExitCode: 1,
			Stderr: "slurm_update error: Invalid node state specified\n",
			Err:    errors.New("login (login.hpc.example.org): command exited 1")}
		_, err := run(t, harnessOptions{recorder: cluster.recorder()},
			"slurm", "node", "resume", "-n", "exe[1-2]", "-y")
		if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
			t.Errorf("exit code = %d, want %d", got, want)
		}
		if err == nil || !strings.Contains(err.Error(), "Invalid node state specified") {
			t.Errorf("error = %v, want Slurm's message", err)
		}
	})

	t.Run("a partial drain", func(t *testing.T) {
		// scontrol update changes the nodes it reaches before it fails.
		cluster := newSlurmCluster()
		cluster.update = &transport.Result{ExitCode: 1, Stderr: "slurm_update error: Invalid node state specified\n"}
		cluster.applied = func(c *slurmCluster) {
			n := c.node("exe0001")
			n["T"], n["E"], n["u"] = "drained", "ticket 42", "root"
		}
		_, err := run(t, harnessOptions{recorder: cluster.recorder()},
			"slurm", "node", "drain", "ticket 42", "-n", "exe[1-2]", "-y")
		if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
			t.Errorf("exit code = %d, want %d", got, want)
		}
		if err == nil || !strings.Contains(err.Error(), "exe0001 is drained with this reason, exe0002 is not") {
			t.Errorf("error = %v, want it to say which nodes changed", err)
		}
	})

	unreachable := func() *transport.Recorder {
		return &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
			return &transport.Result{Target: tg, ExitCode: 255,
				Err: exitcode.Wrap(exitcode.Transport, errors.New("login: Connection refused"))}, nil
		}}
	}
	for _, args := range [][]string{
		{"slurm", "node", "drain", "x", "-n", "exe0001", "-y"},
		{"slurm", "node", "resume", "-n", "exe0001", "-y"},
		{"slurm", "node", "list"},
		{"slurm", "account", "add", "proj", "-y"},
	} {
		t.Run("unreachable "+strings.Join(args[:3], " "), func(t *testing.T) {
			_, err := run(t, harnessOptions{recorder: unreachable()}, args...)
			if got, want := exitcode.From(err), exitcode.Transport; got != want {
				t.Errorf("exit code = %d, want %d (%v)", got, want, err)
			}
		})
	}

	t.Run("a refused read", func(t *testing.T) {
		rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
			return &transport.Result{Target: tg, ExitCode: 1, Stderr: "sinfo: error: Invalid node state specified: bogus\n",
				Err: errors.New("login: command exited 1")}, nil
		}}
		_, err := run(t, harnessOptions{recorder: rec}, "slurm", "node", "list", "--state", "bogus")
		if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
			t.Errorf("exit code = %d, want %d", got, want)
		}
		if err == nil || !strings.Contains(err.Error(), "Invalid node state specified: bogus") {
			t.Errorf("error = %v, want sinfo's message", err)
		}
	})

	t.Run("a user getent does not know", func(t *testing.T) {
		_, err := run(t, harnessOptions{recorder: newSlurmCluster().recorder()}, "slurm", "user", "add", "ghost", "proj", "-y")
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("exit code = %d, want %d (%v)", got, want, err)
		}
		if err == nil || !strings.Contains(err.Error(), `the cluster does not know a user account "ghost"`) {
			t.Errorf("error = %v", err)
		}
	})
}

// 12.4: the start of the window must not depend on the time zone of the
// workstation.
func TestSlurmJobHistoryWindowIsRelative(t *testing.T) {
	for _, tz := range []string{"UTC", "Europe/Berlin", "Asia/Tokyo"} {
		t.Run(tz, func(t *testing.T) {
			t.Setenv("TZ", tz)
			cluster := newSlurmCluster()
			if _, err := run(t, harnessOptions{recorder: cluster.recorder()},
				"slurm", "job", "history", "--state", "failed", "--since", "1h"); err != nil {
				t.Fatalf("history failed: %v", err)
			}
			sent := cluster.sentBy("sacct")
			if len(sent) != 1 || !strings.Contains(sent[0], "--starttime now-3600 ") {
				t.Errorf("sent %v, want the window relative to the login node's clock", sent)
			}
		})
	}
}

// 2.13: the directory check reads the cluster for real in a dry run.
func TestSlurmUserAddDryRunChecksTheDirectory(t *testing.T) {
	cluster := newSlurmCluster()
	h, err := run(t, harnessOptions{recorder: cluster.recorder()}, "slurm", "user", "add", "alice", "proj", "--dry-run")
	if err != nil && !safety.IsDryRun(err) {
		t.Fatalf("the dry run failed: %v", err)
	}
	if len(cluster.sentBy("getent")) != 1 {
		t.Errorf("getent was asked %d times, want once", len(cluster.sentBy("getent")))
	}
	if changes := cluster.changes(); len(changes) != 0 {
		t.Errorf("a dry run sent %v", changes)
	}
	if !strings.Contains(h.errOut.String(), "Would create the Slurm user alice") {
		t.Errorf("the dry run says:\n%s", h.errOut)
	}
}

// 12.5: the default account given for a user who exists already is set.
func TestSlurmUserAddSetsTheDefaultAccountOfAnExistingUser(t *testing.T) {
	cluster := newSlurmCluster()
	cluster.associations = "alice|other|other|1\n"
	h, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: cluster.recorder()},
		"slurm", "user", "add", "alice", "proj", "proj")
	if err != nil {
		t.Fatalf("user add failed: %v", err)
	}
	changes := cluster.changes()
	want := []string{
		"sacctmgr --immediate add user names=alice account=proj cluster=hpc",
		"sacctmgr --immediate modify user where name=alice cluster=hpc set defaultaccount=proj",
	}
	if strings.Join(changes, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent\n%s\nwant\n%s", strings.Join(changes, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(h.errOut.String(), "make proj the default account of alice instead of other") {
		t.Errorf("the preview does not mention the default account:\n%s", h.errOut)
	}
}

// 12.9: the preview says what the defaults resolve to, and the extra
// association a different default account creates.
func TestSlurmUserAddPreviewResolvesTheDefaults(t *testing.T) {
	tests := []struct {
		args []string
		want string
		sent string
	}{
		{[]string{"alice"}, "create the Slurm user alice with the account default",
			"sacctmgr --immediate add user names=alice account=default defaultaccount=default cluster=hpc"},
		{[]string{"alice", "proj", "other"}, "create the Slurm user alice with the accounts proj and other, other the default",
			"sacctmgr --immediate add user names=alice account=proj,other defaultaccount=other cluster=hpc"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cluster := newSlurmCluster()
			args := append([]string{"slurm", "user", "add"}, tc.args...)
			h, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: cluster.recorder()}, args...)
			if err != nil {
				t.Fatalf("user add failed: %v", err)
			}
			if !strings.Contains(h.errOut.String(), tc.want) || !strings.Contains(h.errOut.String(), "in the Slurm cluster hpc") {
				t.Errorf("the preview says:\n%s\nwant %q in the cluster hpc", h.errOut, tc.want)
			}
			if changes := cluster.changes(); len(changes) != 1 || changes[0] != tc.sent {
				t.Errorf("sent %v, want %q", changes, tc.sent)
			}
		})
	}
}

// 12.9: every accounting change names the cluster, so that it does not
// reach every cluster the database serves.
func TestSlurmAccountingChangesNameTheCluster(t *testing.T) {
	tests := []struct {
		args []string
		sent string
	}{
		{[]string{"account", "add", "proj"},
			"sacctmgr --immediate add account proj cluster=hpc description=proj organization=example"},
		{[]string{"account", "shares", "proj", "100"},
			"sacctmgr --immediate modify account where name=proj cluster=hpc set fairshare=100"},
		{[]string{"user", "default", "alice", "proj"},
			"sacctmgr --immediate modify user where name=alice cluster=hpc set defaultaccount=proj"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cluster := newSlurmCluster()
			args := append([]string{"slurm"}, tc.args...)
			h, err := run(t, harnessOptions{tty: true, stdin: "y\n", recorder: cluster.recorder()}, args...)
			if err != nil {
				t.Fatalf("failed: %v", err)
			}
			if changes := cluster.changes(); len(changes) != 1 || changes[0] != tc.sent {
				t.Errorf("sent %v, want %q", changes, tc.sent)
			}
			if !strings.Contains(h.errOut.String(), "in the Slurm cluster hpc") {
				t.Errorf("the preview does not name the cluster:\n%s", h.errOut)
			}
		})
	}
}

// 12.9: sacctmgr expands lists and ranges in names, and getent resolves a
// number as a UID.
func TestSlurmAccountingRefusesNamesSacctmgrExpands(t *testing.T) {
	for _, args := range [][]string{
		{"account", "add", "proj[1-100]"},
		{"account", "add", "a,b"},
		{"account", "coordinator", "proj", "alice,bob"},
		{"account", "shares", "proj[1-2]", "100"},
		{"account", "shares", "proj", "100,parent"},
		{"user", "add", "1000", "proj"},
		{"user", "add", "alice", "proj[1-2]"},
		{"user", "default", "alice", "a,b"},
		{"account", "add", "proj", "example", "line\nbreak"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cluster := newSlurmCluster()
			_, err := run(t, harnessOptions{recorder: cluster.recorder()}, append(append([]string{"slurm"}, args...), "-y")...)
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d (%v)", got, want, err)
			}
			if changes := cluster.changes(); len(changes) != 0 {
				t.Errorf("sent %v", changes)
			}
		})
	}
}
