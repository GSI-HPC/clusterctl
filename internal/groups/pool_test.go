// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package groups_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
	"github.com/GSI-HPC/clusterctl/internal/groups"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

type runnerFunc func(context.Context, transport.Target, transport.Request) (*transport.Result, error)

func (f runnerFunc) Run(ctx context.Context, tg transport.Target, req transport.Request) (*transport.Result, error) {
	return f(ctx, tg, req)
}

// partitionCount is how many groups the source of these tests lists, more
// than fanout.PerHost several times over.
const partitionCount = 12

// listed answers a source that lists its groups, p00 to p11, and maps each
// to one node of its own; it has neither an all nor a reverse command.
func listed(_ context.Context, tg transport.Target, req transport.Request) (*transport.Result, error) {
	if slices.Contains(req.Argv, "%R") {
		var names []string
		for i := range partitionCount {
			names = append(names, fmt.Sprintf("p%02d", i))
		}
		return &transport.Result{Target: tg, Stdout: strings.Join(names, "\n") + "\n"}, nil
	}
	group := req.Argv[len(req.Argv)-1]
	return &transport.Result{Target: tg, Stdout: "exe" + strings.TrimPrefix(group, "p") + "\n"}, nil
}

func listedOptions(t *testing.T, runner transport.Runner) groups.Options {
	opts := testOptions(t, nil, "")
	opts.Runner = runner
	opts.Spec = v1alpha1.GroupsSpec{
		DefaultSource: "slurm",
		Sources: map[string]v1alpha1.GroupSource{
			"slurm": {Exec: &v1alpha1.ExecGroupSource{
				Role: "login",
				Map:  []string{"sinfo", "-h", "-o", "%N", "-p", groups.PlaceholderGroup},
				List: []string{"sinfo", "-h", "-o", "%R"},
			}},
		},
	}
	return opts
}

// @source:* of a source without an all command, and the groups of a node in
// a source without a reverse command, looked up every group of the source
// one after the other: twelve groups, twelve round trips to the login node
// in a row. They are looked up side by side, as many at once as the host
// takes: each command is held until one more than fanout.PerHost are under
// way, which never happens while the bound is kept.
func TestTheGroupsOfASourceAreLookedUpSideBySide(t *testing.T) {
	t.Parallel()
	for name, ask := range map[string]func(*groups.Resolver) (string, error){
		"@slurm:*": func(r *groups.Resolver) (string, error) {
			all, err := r.All("slurm")
			if err != nil {
				return "", err
			}
			ns, err := nodeset.ParseWith(all, r)
			return ns.String(), err
		},
		"the groups of exe07": func(r *groups.Resolver) (string, error) {
			of, err := r.GroupsOf("exe07")
			return strings.Join(of["slurm"], ","), err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := &fanouttest.InFlight{Hold: fanout.PerHost + 1}
			r := groups.New(listedOptions(t, calls.Runner(runnerFunc(listed))))
			got, err := ask(r)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[string]string{"@slurm:*": "exe[00-11]", "the groups of exe07": "p07"}[name]; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
			if peak := calls.Peak(); peak != fanout.PerHost {
				t.Errorf("%d commands were under way at once, want %d", peak, fanout.PerHost)
			}
			if n := calls.Started(); n != partitionCount+1 {
				t.Errorf("%d commands were sent, want the list and one for each group", n)
			}
		})
	}
}

// A command holds a place on every host its session reaches, a jump host
// on the way among them, in the Hosts the resolver is given, which the
// other work of a command shares: while that work holds the jump host's
// every place, no lookup is sent.
func TestALookupTakesItsPlacesInTheHostsItIsGiven(t *testing.T) {
	t.Parallel()
	var sent atomic.Int32
	opts := listedOptions(t, runnerFunc(func(ctx context.Context, tg transport.Target, req transport.Request) (*transport.Result, error) {
		sent.Add(1)
		return listed(ctx, tg, req)
	}))
	opts.Hosts = &fanout.Hosts{}
	opts.On = func(tg transport.Target) []string { return []string{"jump.example.org", tg.Host} }
	release, err := opts.Hosts.Acquire(context.Background(), "jump.example.org")
	if err != nil {
		t.Fatal(err)
	}
	var others []func()
	for range fanout.PerHost - 1 {
		more, err := opts.Hosts.Acquire(context.Background(), "jump.example.org")
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, more)
	}

	done := make(chan error, 1)
	go func() {
		_, err := groups.New(opts).Resolve("slurm", "p03")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the lookup ended while the jump host had no place: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if n := sent.Load(); n != 0 {
		t.Fatalf("%d commands were sent while the jump host had no place", n)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, r := range others {
		r()
	}
	if n := sent.Load(); n != 1 {
		t.Errorf("%d commands were sent, want 1", n)
	}
}

// @source:* stops at the first group it cannot look up, as it did when it
// looked them up one after the other: no further lookup is started, those
// under way are ended, and what is reported is the failure, not the end of
// the others.
func TestAllStopsAtTheFirstGroupItCannotLookUp(t *testing.T) {
	t.Parallel()
	var sent atomic.Int32
	r := groups.New(listedOptions(t, runnerFunc(func(ctx context.Context, tg transport.Target, req transport.Request) (*transport.Result, error) {
		if slices.Contains(req.Argv, "%R") {
			return listed(ctx, tg, req)
		}
		sent.Add(1)
		if req.Argv[len(req.Argv)-1] == "p02" {
			time.Sleep(20 * time.Millisecond)
			return transport.ExitResult(tg, 1, "", "sinfo: error: Unable to contact slurm controller\n"), nil
		}
		<-ctx.Done()
		return &transport.Result{Target: tg, ExitCode: -1, Err: fmt.Errorf("%s: %w", tg, ctx.Err())}, nil
	})))
	_, err := r.All("slurm")
	if got := exitcode.From(err); got != exitcode.TargetFailed || !strings.Contains(err.Error(), "Unable to contact slurm controller") {
		t.Errorf("All: exit code %d (%v), want the failure of p02", got, err)
	}
	if n := sent.Load(); n != fanout.PerHost {
		t.Errorf("%d groups were looked up, want only the %d under way when p02 failed", n, fanout.PerHost)
	}
}

// The groups of a node in a source of node attributes were found by
// expanding every group of the source, one for each value, each a pass
// over the inventory: with a value for each of 20,000 nodes, minutes. The
// node's own value answers, and says what expanding every group said.
func TestTheGroupsOfANodeInAnAttributeSourceAreItsValue(t *testing.T) {
	t.Parallel()
	var entries []v1alpha1.NodeEntry
	for i := range 20000 {
		entries = append(entries, v1alpha1.NodeEntry{
			Nodes:      fmt.Sprintf("n%05d", i),
			Attributes: map[string]string{"serial": fmt.Sprintf("s%05d", i), "rack": fmt.Sprintf("r%02d", i%50)},
		})
	}
	inv, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: entries})
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions(t, nil, "")
	opts.Inventory = inv
	opts.Spec = v1alpha1.GroupsSpec{DefaultSource: "serial", Sources: map[string]v1alpha1.GroupSource{
		"serial": {Attribute: "serial"},
		"rack":   {Attribute: "rack"},
		"none":   {Attribute: "absent"},
	}}
	r := groups.New(opts)
	start := time.Now()
	for _, node := range []string{"n12345", "n00007", "n7"} {
		got, err := r.GroupsOf(node)
		if err != nil {
			t.Fatal(err)
		}
		var want map[string][]string
		switch node {
		case "n12345":
			want = map[string][]string{"serial": {"s12345"}, "rack": {"r45"}}
		default:
			want = map[string][]string{"serial": {"s00007"}, "rack": {"r07"}}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("GroupsOf(%s) = %v, want %v", node, got, want)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the groups of three nodes took %v", elapsed)
	}
	// What the groups say when expanded agrees with the node's value.
	ns, err := nodeset.ParseWith("@rack:r45", r)
	if err != nil || !ns.Contains("n12345") || ns.Len() != 400 {
		t.Errorf("@rack:r45 = %v (%v), want 400 nodes, n12345 among them", ns, err)
	}
}
