// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// nodes names n nodes, exe1 to exeN.
func nodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("exe%d", i+1)
	}
	return out
}

// watch returns a context whose Bus sends to a capture. close closes the
// Bus, checks every promise the events make and returns their tree.
func watch(t *testing.T) (ctx context.Context, tree func() string) {
	t.Helper()
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	return progress.WithBus(context.Background(), bus), func() string {
		t.Helper()
		bus.Close()
		progresstest.Check(t, c.Events())
		return c.Tree()
	}
}

// onBMC describes a node by its service processor.
func onBMC(node string) (string, string, string) { return node, node + ".mgmt.example.org", "" }

func TestMapKeepsTheOrderOfTheItems(t *testing.T) {
	t.Parallel()

	items := nodes(5)
	outcomes := fanout.Map(context.Background(), items, fanout.Options[string]{Limit: 5},
		func(_ context.Context, node string) (string, error) {
			// The first finishes last, which must not move it.
			if node == "exe1" {
				time.Sleep(20 * time.Millisecond)
			}
			return strings.ToUpper(node), nil
		})
	var got []string
	for _, o := range outcomes {
		if !o.Started || o.Err != nil {
			t.Errorf("outcome %+v, want it started and without an error", o)
		}
		got = append(got, o.Value)
	}
	if want := "EXE1,EXE2,EXE3,EXE4,EXE5"; strings.Join(got, ",") != want {
		t.Errorf("values = %v, want %s", got, want)
	}
}

// Every call is held until one more than the limit are under way, which a
// pool keeping to its limit never allows, so exactly the limit run at once.
func TestMapKeepsToItsLimit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		limit, items int
		want         int
	}{
		{"one at a time", 1, 4, 1},
		{"three at a time", 3, 9, 3},
		{"no limit given", 0, fanout.DefaultMax + 2, fanout.DefaultMax},
		{"more room than items", 8, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := &fanouttest.InFlight{Hold: tc.want + 1}
			ctx, tree := watch(t)
			fanout.Map(ctx, nodes(tc.items), fanout.Options[string]{Step: "scan", Limit: tc.limit},
				func(context.Context, string) (struct{}, error) {
					defer calls.Enter()()
					return struct{}{}, nil
				})
			// Check holds the running targets to the step's Limit too.
			tree()
			if got := calls.Peak(); got != tc.want {
				t.Errorf("%d items ran at once, want %d", got, tc.want)
			}
			if got := calls.Started(); got != tc.items {
				t.Errorf("%d items ran, want %d", got, tc.items)
			}
		})
	}
}

// Once the context has ended no item is started, and each left out is
// reported with the context's error: never started, however the free
// places and the cancellation raced.
func TestMapStartsNothingOnceCancelled(t *testing.T) {
	t.Parallel()

	t.Run("before the run", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var ran atomic.Int32
		outcomes := fanout.Map(ctx, nodes(200), fanout.Options[string]{Limit: 4},
			func(context.Context, string) (int, error) { ran.Add(1); return 0, nil })
		if got := ran.Load(); got != 0 {
			t.Errorf("%d items were started on a cancelled context", got)
		}
		for i, o := range outcomes {
			if o.Started || !errors.Is(o.Err, context.Canceled) {
				t.Fatalf("outcome %d = %+v, want it not started and cancelled", i, o)
			}
		}
	})

	t.Run("during the run", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		failed := errors.New("exe2: command exited -1")
		outcomes := fanout.Map(ctx, nodes(200), fanout.Options[string]{Limit: 1},
			func(_ context.Context, node string) (string, error) {
				if node == "exe2" {
					cancel()
					return "", failed
				}
				return "ok", nil
			})
		started, cancelled := 0, 0
		for _, o := range outcomes {
			switch {
			case o.Started:
				started++
			case errors.Is(o.Err, context.Canceled):
				cancelled++
			default:
				t.Errorf("outcome %+v is neither started nor cancelled", o)
			}
		}
		if started != 2 || cancelled != 198 {
			t.Errorf("%d started and %d cancelled, want the 2 before the interrupt and the 198 after it", started, cancelled)
		}
		// The item the interrupt came during keeps what it said.
		if !errors.Is(outcomes[1].Err, failed) {
			t.Errorf("exe2: error = %v, want its own", outcomes[1].Err)
		}
	})
}

// A panic in the work for one item is that item's failure, and the rest
// finish; the stack goes to the pool's log.
func TestMapTurnsAPanicIntoThatItemsFailure(t *testing.T) {
	t.Parallel()

	var log strings.Builder
	outcomes := fanout.Map(context.Background(), nodes(3), fanout.Options[string]{Limit: 2, PanicLog: &log},
		func(_ context.Context, node string) (*string, error) {
			if node == "exe2" {
				panic("assignment to entry in nil map")
			}
			return &node, nil
		})
	for i, o := range outcomes {
		if i == 1 {
			if o.Value != nil || exitcode.From(o.Err) != exitcode.TargetFailed || !strings.Contains(fmt.Sprint(o.Err), "panicked") {
				t.Errorf("exe2 = %+v, want no value and an error saying clusterctl panicked", o)
			}
			continue
		}
		if o.Err != nil || o.Value == nil {
			t.Errorf("exe%d = %+v, want it to have finished", i+1, o)
		}
	}
	if !strings.Contains(log.String(), "exe2") || !strings.Contains(log.String(), "goroutine") {
		t.Errorf("the log has no stack naming exe2:\n%s", log.String())
	}
}

// A pool reports a step with its total and limit, and every target queued
// before the first runs; the calls made for a target nest under it, and the
// step says which failed.
func TestMapReportsItsWork(t *testing.T) {
	t.Parallel()

	ctx, tree := watch(t)
	fanout.Map(ctx, nodes(5), fanout.Options[string]{Step: "reset the machines", Limit: 2, Describe: onBMC},
		func(ctx context.Context, node string) (struct{}, error) {
			_, call := progress.Start(ctx, progress.KindCall, "redfish", progress.HTTP("POST", "/redfish/v1/Systems/1"))
			var err error
			if node == "exe3" {
				err = exitcode.Wrap(exitcode.Transport, errors.New(node+".mgmt.example.org: connection refused"))
			}
			call.End(err)
			return struct{}{}, err
		})
	want := `step reset the machines total=5 limit=2 [fold]: failed (transport): 1 of 5 failed: exe3
  target exe3: failed (transport): {}: connection refused
    call redfish method=POST path=/redfish/v1/Systems/1: failed (transport): {}: connection refused
  target exe[1-2,4-5]: ok
    call redfish method=POST path=/redfish/v1/Systems/1: ok
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// An interrupt leaves every target it kept from starting canceled, so the
// count still reaches the total, and the target it came during is canceled
// too, whatever its work made of it.
func TestMapReportsAnInterrupt(t *testing.T) {
	t.Parallel()

	ctx, tree := watch(t)
	ctx, cancel := context.WithCancel(ctx)
	fanout.Map(ctx, nodes(6), fanout.Options[string]{Step: "reset the machines", Limit: 1},
		func(_ context.Context, node string) (struct{}, error) {
			if node == "exe2" {
				cancel()
				return struct{}{}, errors.New("exe2: command exited -1")
			}
			return struct{}{}, nil
		})
	want := `step reset the machines total=6 limit=1 [fold]: canceled (canceled): 5 of 6 failed: exe[2-6]
  target exe1: ok
  target exe[2-6]: canceled (canceled): context canceled
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// A target that failed once the interrupt had come ends canceled, and a
// step whose failures all did ends canceled too, whatever the work made of
// the interrupt.
func TestMapEndsAStepTheInterruptEndedCanceled(t *testing.T) {
	t.Parallel()

	ctx, tree := watch(t)
	ctx, cancel := context.WithCancel(ctx)
	fanout.Map(ctx, nodes(2), fanout.Options[string]{Step: "read the power state", Limit: 2},
		func(_ context.Context, node string) (struct{}, error) {
			if node == "exe2" {
				cancel()
				return struct{}{}, exitcode.Errorf(exitcode.Transport, "exe2: dial tcp: operation was canceled")
			}
			return struct{}{}, nil
		})
	want := `step read the power state total=2 limit=2 [fold]: canceled (canceled): 1 of 2 failed: exe2
  target exe1: ok
  target exe2: canceled (canceled): context canceled
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// A step says why it failed as the exit code of its failures does, not as
// the first of them that says a class of its own: a timeout among hosts
// that could not be reached makes the step's code Transport.
func TestMapStepSaysTheClassOfItsExitCode(t *testing.T) {
	t.Parallel()

	ctx, tree := watch(t)
	fanout.Map(ctx, nodes(3), fanout.Options[string]{Step: "copy", Limit: 1},
		func(_ context.Context, node string) (struct{}, error) {
			switch node {
			case "exe1":
				return struct{}{}, exitcode.Wrap(exitcode.Transport, fmt.Errorf("exe1: %w", context.DeadlineExceeded))
			case "exe2":
				return struct{}{}, exitcode.Errorf(exitcode.Transport, "exe2: connection refused")
			}
			return struct{}{}, nil
		})
	want := `step copy total=3 limit=1 [fold]: failed (transport): 2 of 3 failed: exe[1-2]
  target exe1: failed (timeout): {}: context deadline exceeded
  target exe2: failed (transport): {}: connection refused
  target exe3: ok
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// The executor reports its targets through Map: under the step its caller
// names, or "run", with the flags it is given. A command that exited
// non-zero without an error is a failed target all the same.
func TestExecutorReportsItsTargets(t *testing.T) {
	t.Parallel()

	ctx, tree := watch(t)
	rec := &transport.Recorder{ByTarget: map[string]*transport.Result{"exe2": {ExitCode: 1}}}
	e := &fanout.Executor{Runner: rec, Max: 2, Flags: progress.ShowLines}
	results := e.Run(ctx, targets("exe1", "exe2", "exe3"), transport.Request{Argv: []string{"uptime"}})
	if got := len(fanout.Failures(results)); got != 1 {
		t.Errorf("%d targets failed, want 1", got)
	}
	want := `step run total=3 limit=2 [fold,show-lines]: failed (target): 1 of 3 failed: exe2
  target exe2 [show-lines]: failed (target): {}: command exited 1
  target exe[1,3] [show-lines]: ok
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// The error a fan-out exits with counts and names the hosts that failed,
// keeps the exit code the worst of them asks for, and the progress class of
// that code, and keeps their errors underneath.
func TestFailureError(t *testing.T) {
	t.Parallel()

	down := exitcode.Wrap(exitcode.Transport, errors.New("exe2: Connection refused"))
	for _, tc := range []struct {
		name    string
		results []*transport.Result
		want    string
		code    int
		class   progress.Class
	}{
		{"none failed", []*transport.Result{{Target: transport.Target{Name: "exe1"}}}, "", exitcode.OK, progress.ClassNone},
		{"a command that exited non-zero", []*transport.Result{
			{Target: transport.Target{Name: "exe1"}},
			{Target: transport.Target{Name: "exe2"}, ExitCode: 1},
		}, "1 of 2 hosts failed: exe2", exitcode.TargetFailed, progress.ClassTarget},
		{"an unreachable host among them", []*transport.Result{
			{Target: transport.Target{Name: "exe1"}, ExitCode: 1, Err: errors.New("exe1: command exited 1")},
			{Target: transport.Target{Name: "exe2"}, ExitCode: 255, Err: down},
			{Target: transport.Target{Name: "exe3"}},
		}, "2 of 3 hosts failed: exe[1-2]", exitcode.Transport, progress.ClassTransport},
		{"a timeout among them", []*transport.Result{
			{Target: transport.Target{Name: "exe1"}, ExitCode: 124, Err: fmt.Errorf("exe1: %w", context.DeadlineExceeded)},
			{Target: transport.Target{Name: "exe2"}, ExitCode: 255, Err: down},
		}, "2 of 2 hosts failed: exe[1-2]", exitcode.Transport, progress.ClassTransport},
		{"an interrupt", []*transport.Result{
			{Target: transport.Target{Name: "exe1"}, ExitCode: 255, Err: down},
			{Target: transport.Target{Name: "exe2"}, ExitCode: -1, Err: context.Canceled},
		}, "2 of 2 hosts failed: exe[1-2]", exitcode.Interrupted, progress.ClassCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := fanout.FailureError(tc.results)
			if got := fmt.Sprint(err); tc.want != "" && got != tc.want || tc.want == "" && err != nil {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
			if got := exitcode.From(err); got != tc.code {
				t.Errorf("exit code %d, want %d", got, tc.code)
			}
			if got := progress.Classify(err); got != tc.class {
				t.Errorf("class %s, want %s", got, tc.class)
			}
			if tc.code == exitcode.Transport && !errors.Is(err, down) {
				t.Errorf("the error of the unreachable host is not kept underneath: %v", err)
			}
		})
	}
}
