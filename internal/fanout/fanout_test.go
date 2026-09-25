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
	for i := range 20 {
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

// TestRunStartsNothingOnceCancelled checks that no target is handed to the
// runner after the context has ended. With a free slot and a cancelled
// context both ready, select picked either at random, so about half the
// remaining targets were still started.
func TestRunStartsNothingOnceCancelled(t *testing.T) {
	t.Parallel()

	names := make([]string, 200)
	for i := range names {
		names[i] = fmt.Sprintf("exe%d", i+1)
	}

	t.Run("before the run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := &transport.Recorder{}
		results := (&fanout.Executor{Runner: rec, Max: 4}).Run(ctx, targets(names...), transport.Request{Argv: []string{"true"}})
		if got := len(rec.Calls()); got != 0 {
			t.Errorf("%d targets were started on a cancelled context", got)
		}
		for i, r := range results {
			if r == nil || !errors.Is(r.Err, context.Canceled) {
				t.Fatalf("result %d = %+v, want it reported as cancelled", i, r)
			}
		}
	})

	t.Run("during the run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
			cancel()
			return &transport.Result{Target: tg}, nil
		}}
		(&fanout.Executor{Runner: rec, Max: 1}).Run(ctx, targets(names...), transport.Request{Argv: []string{"true"}})
		if got := len(rec.Calls()); got != 1 {
			t.Errorf("%d targets were started, want only the one running when the context was cancelled", got)
		}
	})
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
	groups, err := fanout.GroupByOutput(results)
	if err != nil {
		t.Fatal(err)
	}
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
	groups, err := fanout.GroupByOutput(results)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, g := range groups {
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

// A panic in the work for one target ended the process, and with it
// clusterctl mcp and every plan it held: recover only catches a panic in
// its own goroutine. It is now that target's failure, and the rest finish.
// The stack goes to the executor's log, which is the front end's
// diagnostics, rather than always to the process's standard error.
func TestRunTurnsAPanicIntoThatTargetsFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(*fanout.Executor)
	}{
		{"in the runner", func(e *fanout.Executor) {
			e.Runner = &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
				if tg.Name == "exe2" {
					panic("index out of range [3] with length 3")
				}
				return &transport.Result{Target: tg, Stdout: "ok\n"}, nil
			}}
		}},
		{"in OnResult", func(e *fanout.Executor) {
			e.OnResult = func(r *transport.Result) {
				if r.Target.Name == "exe2" {
					panic("index out of range [3] with length 3")
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var log strings.Builder
			e := &fanout.Executor{Runner: &transport.Recorder{}, Max: 2, PanicLog: &log}
			tc.setup(e)

			results := e.Run(context.Background(), targets("exe1", "exe2", "exe3"), transport.Request{Argv: []string{"true"}})
			if len(results) != 3 {
				t.Fatalf("got %d results, want 3", len(results))
			}
			for _, r := range results {
				if r.Target.Name == "exe2" {
					if !r.Failed() || exitcode.From(r.Err) != exitcode.TargetFailed {
						t.Errorf("exe2 = %+v, want it failed with exit code 1", r)
					}
					if r.Err == nil || !strings.Contains(r.Err.Error(), "panicked") {
						t.Errorf("exe2: error = %v, want it to say clusterctl panicked", r.Err)
					}
					continue
				}
				if r.Failed() {
					t.Errorf("%s failed: %v", r.Target.Name, r.Err)
				}
			}
			if !strings.Contains(log.String(), "exe2") || !strings.Contains(log.String(), "goroutine") {
				t.Errorf("the log has no stack naming exe2:\n%s", log.String())
			}
		})
	}
}

// A target name was added to a group as a node set expression, and one the
// set refused was dropped although the comment said it became its own
// group: exe[2-3] was counted as two hosts, and a,b[ vanished. A name that
// is not one host name cannot be grouped, which is reported instead.
func TestGroupByOutputRefusesWhatIsNotAHostName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"exe[2-3]", "exe2,exe3", "a,b["} {
		results := []*transport.Result{
			{Target: transport.Target{Name: "exe1"}, Stdout: "yes\n"},
			{Target: transport.Target{Name: name}, Stdout: "yes\n"},
		}
		groups, err := fanout.GroupByOutput(results)
		if err == nil {
			t.Errorf("%q: grouped as %v, want an error", name, groups)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%q: error = %v, want it to name the target", name, err)
		}
	}
}
