// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// reverseLookup reports whether a request is the example slurm source's
// question for the partitions of one node, and names the node.
func reverseLookup(req transport.Request) (string, bool) {
	if len(req.Argv) == 0 || req.Argv[0] != "sinfo" || !slices.Contains(req.Argv, "%R") {
		return "", false
	}
	node := slurm.Arg(req, "-n")
	return node, node != ""
}

// partitions answers the example slurm source's reverse lookups with the
// partition main, or with fail's error for a node it names, and hands
// every other request to next.
func partitions(next transport.Runner, fail func(node string) (string, time.Duration)) transport.Runner {
	return runnerFunc(func(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
		node, ok := reverseLookup(req)
		if !ok {
			return next.Run(ctx, target, req)
		}
		if fail != nil {
			if msg, after := fail(node); msg != "" {
				time.Sleep(after)
				return transport.ExitResult(target, 1, "", msg+"\n"), nil
			}
		}
		return &transport.Result{Target: target, Stdout: "main\n"}, nil
	})
}

type describedGroups struct {
	Items []struct {
		Name   string              `json:"name"`
		Groups map[string][]string `json:"groups"`
		Slurm  *struct {
			Reason string `json:"reason"`
		} `json:"slurm"`
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
	} `json:"items"`
	Errors map[string]string `json:"errors"`
}

// describe_nodes read the groups of one node after the other, and then
// asked sinfo and squeue one after the other too. The groups are read on a
// pool, and sinfo and squeue beside it, and the example's slurm source asks
// the login node, where the Slurm clients run: the call opens at most
// fanout.PerHost sessions to it at a time between them. Each session is
// held until one more than that are under way, which never happens while
// the bound is kept, so exactly the bound are open at once. What the call
// returns is what it returned before.
func TestDescribeNodesReadsItsFacetsAtOnceWithinTheHostsBound(t *testing.T) {
	calls := &fanouttest.InFlight{Hold: fanout.PerHost + 1}
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	f := start(t, setup{
		ctx: progress.WithBus(context.Background(), bus),
		runner: func(next transport.Runner) transport.Runner {
			return calls.Runner(partitions(next, nil))
		},
	})
	var out describedGroups
	f.call(t, "describe_nodes", map[string]any{"nodes": "exe[1-6]", "facets": []string{"groups", "slurm", "jobs"}}, &out)
	bus.Close()
	progresstest.Check(t, c.Events())

	if got := calls.Peak(); got != fanout.PerHost {
		t.Errorf("%d sessions were open to the login node at once, want %d", got, fanout.PerHost)
	}
	if got := calls.Started(); got != 8 {
		t.Errorf("%d sessions were opened, want one for the groups of each of 6 nodes, sinfo and squeue", got)
	}
	if len(out.Items) != 6 || len(out.Errors) != 0 {
		t.Fatalf("items = %+v, errors = %v", out.Items, out.Errors)
	}
	for _, item := range out.Items {
		if got := strings.Join(item.Groups["slurm"], ","); got != "main" {
			t.Errorf("%s is in the Slurm partitions %q, want main", item.Name, got)
		}
		if got := strings.Join(item.Groups["inventory"], ","); got != "exe" {
			t.Errorf("%s is in the inventory groups %q, want exe", item.Name, got)
		}
	}
	if s := out.Items[0].Slurm; s == nil || s.Reason != "ticket 4711: DIMM" {
		t.Errorf("exe0001 slurm = %+v, want the drain reason", s)
	}
	if jobs := out.Items[1].Jobs; len(jobs) != 1 || jobs[0].ID != "4711" {
		t.Errorf("exe0002 jobs = %+v, want job 4711", jobs)
	}
	if !strings.Contains(c.Tree(), "step read the groups total=6 limit=4 [fold]: ok") {
		t.Errorf("the groups are not read as a pool:\n%s", c.Tree())
	}
	// Each node's lookups sit under its target; only sinfo and squeue are
	// the command's own.
	if n := strings.Count(c.Tree(), "\n  call ssh "); n != 2 {
		t.Errorf("%d ssh calls sit outside the targets, want sinfo and squeue alone:\n%s", n, c.Tree())
	}
}

// The groups of the nodes after the first that failed were not read at
// all. Every node's groups are read, a failing source does not hide what
// the others found, and the error reported is the first in the order of
// the nodes, whichever failed first.
func TestDescribeNodesReportsTheFirstGroupFailureInTheOrderOfTheNodes(t *testing.T) {
	f := start(t, setup{runner: func(next transport.Runner) transport.Runner {
		return partitions(next, func(node string) (string, time.Duration) {
			switch node {
			case "exe0002":
				return "sinfo: exe0002 is not answering", 100 * time.Millisecond
			case "exe0004":
				return "sinfo: exe0004 is not answering", 0
			}
			return "", 0
		})
	}})
	var out describedGroups
	f.call(t, "describe_nodes", map[string]any{"nodes": "exe[1-5]", "facets": []string{"groups"}}, &out)
	if msg := out.Errors["groups"]; !strings.Contains(msg, "exe0002 is not answering") || strings.Contains(msg, "exe0004") {
		t.Errorf("errors = %v, want exe0002's failure alone", out.Errors)
	}
	for _, item := range out.Items {
		want := "main"
		if item.Name == "exe0002" || item.Name == "exe0004" {
			want = ""
		}
		if got := strings.Join(item.Groups["slurm"], ","); got != want {
			t.Errorf("%s is in the Slurm partitions %q, want %q", item.Name, got, want)
		}
		if got := strings.Join(item.Groups["inventory"], ","); got != "exe" {
			t.Errorf("%s is in the inventory groups %q, want exe, whatever the slurm source said", item.Name, got)
		}
	}
}

// plan_change asked for the state of the nodes and then for their jobs.
// It asks for both at once, each held until the other is under way, and a
// failure of either is reported in the order it was before: the nodes'
// first, whichever failed first.
func TestPlanReadsTheNodesAndTheJobsAtOnce(t *testing.T) {
	t.Run("both answer", func(t *testing.T) {
		calls := &fanouttest.InFlight{Hold: 2}
		f := start(t, setup{runner: func(next transport.Runner) transport.Runner {
			return runnerFunc(func(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
				if stateOrJobs(req) {
					defer calls.Enter()()
				}
				return next.Run(ctx, target, req)
			})
		}})
		p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe[1-3]"})
		if got := calls.Peak(); got != 2 {
			t.Errorf("%d of sinfo and squeue ran at once, want both", got)
		}
		if p.CurrentState["drained"] != "exe0001" || len(p.Warnings) == 0 {
			t.Errorf("plan = %+v, want the current state and the warnings it gives", p)
		}
	})

	t.Run("both fail", func(t *testing.T) {
		f := start(t, setup{runner: func(next transport.Runner) transport.Runner {
			return runnerFunc(func(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
				switch {
				case !stateOrJobs(req):
					return next.Run(ctx, target, req)
				case req.Argv[0] == "sinfo":
					time.Sleep(100 * time.Millisecond)
					return transport.ExitResult(target, 1, "", "sinfo: slurmctld is not answering\n"), nil
				default:
					return transport.ExitResult(target, 1, "", "squeue: slurmctld is not answering\n"), nil
				}
			})
		}})
		p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe[1-3]"})
		if len(p.Warnings) != 1 {
			t.Fatalf("warnings = %q, want the one saying the state could not be read", p.Warnings)
		}
		w := p.Warnings[0]
		nodes, jobs := strings.Index(w, "sinfo: slurmctld"), strings.Index(w, "squeue: slurmctld")
		if !strings.HasPrefix(w, "the current state could not be read: ") || nodes < 0 || jobs < nodes {
			t.Errorf("warning = %q, want sinfo's failure and then squeue's", w)
		}
		if p.CurrentState != nil {
			t.Errorf("current state = %v, want none", p.CurrentState)
		}
	})
}

// stateOrJobs reports whether a request reads the state of nodes or their
// jobs, rather than checking that Slurm reads a set as itself.
func stateOrJobs(req transport.Request) bool {
	if len(req.Argv) == 0 {
		return false
	}
	return req.Argv[0] == "squeue" || req.Argv[0] == "sinfo" && !slices.Contains(req.Argv, "--all")
}
