// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func targets(names ...string) []transport.Target {
	out := make([]transport.Target, len(names))
	for i, n := range names {
		out[i] = transport.Target{Name: n, Host: n + ".example.org"}
	}
	return out
}

func TestRunKeepsTargetOrder(t *testing.T) {
	t.Parallel()

	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		// Finishing out of order must not change the order of the results.
		if tg.Name == "exe1" {
			time.Sleep(20 * time.Millisecond)
		}
		return &transport.Result{Target: tg, Stdout: tg.Name + "\n"}, nil
	}}
	e := &fanout.Executor{Runner: rec, Max: 4}

	results := e.Run(context.Background(), targets("exe1", "exe2", "exe3"), transport.Request{Argv: []string{"hostname"}})
	var got []string
	for _, r := range results {
		got = append(got, r.Output())
	}
	if want := "exe1,exe2,exe3"; strings.Join(got, ",") != want {
		t.Errorf("results = %q, want %q", got, want)
	}
}

func TestRunBoundsConcurrency(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		running int
		peak    int
	)
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return &transport.Result{Target: tg}, nil
	}}

	e := &fanout.Executor{Runner: rec, Max: 3}
	var all []transport.Target
	for i := 0; i < 20; i++ {
		all = append(all, transport.Target{Name: fmt.Sprintf("exe%d", i)})
	}
	e.Run(context.Background(), all, transport.Request{Argv: []string{"true"}})

	mu.Lock()
	defer mu.Unlock()
	if peak > 3 {
		t.Errorf("%d targets ran at once, want at most 3", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency was %d; the work did not run in parallel", peak)
	}
}

func TestRunReportsEveryTargetOnFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection refused")
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if tg.Name == "exe2" {
			return &transport.Result{Target: tg, ExitCode: 255, Err: boom}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	e := &fanout.Executor{Runner: rec}

	results := e.Run(context.Background(), targets("exe1", "exe2", "exe3"), transport.Request{Argv: []string{"true"}})
	if got, want := len(results), 3; got != want {
		t.Fatalf("got %d results, want %d; one failure must not stop the rest", got, want)
	}
	failures := fanout.Failures(results)
	if got, want := len(failures), 1; got != want {
		t.Errorf("got %d failures, want %d", got, want)
	}
	if got, want := fanout.Succeeded(results).String(), "exe[1,3]"; got != want {
		t.Errorf("Succeeded = %q, want %q", got, want)
	}
}

func TestRunHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if started.Add(1) == 1 {
			cancel()
		}
		time.Sleep(time.Millisecond)
		return &transport.Result{Target: tg}, nil
	}}
	e := &fanout.Executor{Runner: rec, Max: 1}

	results := e.Run(ctx, targets("exe1", "exe2", "exe3", "exe4"), transport.Request{Argv: []string{"true"}})
	for i, r := range results {
		if r == nil {
			t.Fatalf("result %d is nil; a cancelled target must still be reported", i)
		}
	}
	if len(fanout.Failures(results)) == 0 {
		t.Error("cancelling should leave at least one target reported as failed")
	}
}

func TestOnResultIsCalledForEveryTarget(t *testing.T) {
	t.Parallel()

	var seen atomic.Int32
	e := &fanout.Executor{
		Runner:   &transport.Recorder{},
		OnResult: func(*transport.Result) { seen.Add(1) },
	}
	e.Run(context.Background(), targets("exe1", "exe2"), transport.Request{Argv: []string{"true"}})
	if got, want := seen.Load(), int32(2); got != want {
		t.Errorf("OnResult was called %d times, want %d", got, want)
	}
}

func TestGroupByOutput(t *testing.T) {
	t.Parallel()

	results := []*transport.Result{
		{Target: transport.Target{Name: "exe1"}, Stdout: "5.14.0\n"},
		{Target: transport.Target{Name: "exe2"}, Stdout: "5.14.0\n"},
		{Target: transport.Target{Name: "exe3"}, Stdout: "5.14.0\n"},
		{Target: transport.Target{Name: "exe4"}, Stdout: "4.18.0\n"},
		{Target: transport.Target{Name: "exe5"}, Stdout: "", ExitCode: 1},
	}
	groups := fanout.GroupByOutput(results)
	if got, want := len(groups), 3; got != want {
		t.Fatalf("got %d groups, want %d", got, want)
	}
	// The largest group comes first, so the common answer is read first.
	if got, want := groups[0].Nodes.String(), "exe[1-3]"; got != want {
		t.Errorf("first group = %q, want %q", got, want)
	}
	if got, want := groups[0].Output, "5.14.0\n"; got != want {
		t.Errorf("first output = %q, want %q", got, want)
	}
	if groups[2].ExitCode == 0 && groups[1].ExitCode == 0 {
		t.Error("the failing target should form its own group")
	}
}

func TestRunWithNoTargets(t *testing.T) {
	t.Parallel()

	e := &fanout.Executor{Runner: &transport.Recorder{}}
	if got := e.Run(context.Background(), nil, transport.Request{}); len(got) != 0 {
		t.Errorf("got %d results for no targets", len(got))
	}
}

func TestGroupByOutputKeepsApartWhatEndedDifferently(t *testing.T) {
	t.Parallel()

	down := exitcode.Wrap(exitcode.Transport, errors.New("exe4: Connection refused"))
	results := []*transport.Result{
		{Target: transport.Target{Name: "exe1"}, Stdout: "yes\n"},
		{Target: transport.Target{Name: "exe2"}, Stdout: "yes\n\n"},
		{Target: transport.Target{Name: "exe3"}, ExitCode: 1, Err: errors.New("exe3: command exited 1")},
		{Target: transport.Target{Name: "exe4"}, ExitCode: 255, Err: down},
		{Target: transport.Target{Name: "exe5"}, ExitCode: 255, Err: errors.New("exe5: command exited 255")},
		{Target: transport.Target{Name: "exe6"}, ExitCode: -1, Err: context.Canceled},
		{Target: transport.Target{Name: "exe7"}},
	}
	status := map[string]string{}
	for _, g := range fanout.GroupByOutput(results) {
		if g.Nodes.Len() != 1 {
			t.Errorf("%s were grouped although they ended differently", g.Nodes)
		}
		status[g.Nodes.String()] = g.Status
	}
	want := map[string]string{
		"exe1": "ok", "exe2": "ok", "exe3": "exit 1", "exe4": "unreachable",
		"exe5": "exit 255", "exe6": "interrupted", "exe7": "ok",
	}
	for node, w := range want {
		if got := status[node]; got != w {
			t.Errorf("status of %s = %q, want %q", node, got, w)
		}
	}
}
