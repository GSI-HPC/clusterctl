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
	"testing/synctest"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/clikit/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
)

// nodes names n nodes, exe1 to exeN.
func nodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("exe%d", i+1)
	}
	return out
}

// unreachable is the error of a host that could not be reached, which says
// its class itself, as the errors of a program's transport do.
type unreachable struct{ error }

func (unreachable) ProgressClass() progress.Class { return progress.ClassTransport }

func (e unreachable) Unwrap() error { return e.error }

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

// Each call waits a second of the fake clock of testing/synctest, which
// passes only once every call that can start has, so the most under way at
// once is the limit, exactly.
func TestMapKeepsToItsLimit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		limit, items int
		want         int32
	}{
		{"one at a time", 1, 4, 1},
		{"three at a time", 3, 9, 3},
		{"no limit given", 0, fanout.DefaultMax + 2, fanout.DefaultMax},
		{"more room than items", 8, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var now, peak, started atomic.Int32
				ctx, tree := progresstest.Watch(context.Background(), t)
				fanout.Map(ctx, nodes(tc.items), fanout.Options[string]{Step: "scan", Limit: tc.limit},
					func(context.Context, string) (struct{}, error) {
						started.Add(1)
						n := now.Add(1)
						for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
						}
						time.Sleep(time.Second)
						now.Add(-1)
						return struct{}{}, nil
					})
				// Check holds the running targets to the step's Limit too.
				tree()
				if got := peak.Load(); got != tc.want {
					t.Errorf("%d items ran at once, want %d", got, tc.want)
				}
				if got := started.Load(); got != int32(tc.items) {
					t.Errorf("%d items ran, want %d", got, tc.items)
				}
			})
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
	outcomes := fanout.Map(context.Background(), nodes(3), fanout.Options[string]{Limit: 2, PanicLog: &log, Program: "sind"},
		func(_ context.Context, node string) (*string, error) {
			if node == "exe2" {
				panic("assignment to entry in nil map")
			}
			return &node, nil
		})
	for i, o := range outcomes {
		if i == 1 {
			var p *fanout.PanicError
			if o.Value != nil || !errors.As(o.Err, &p) || p.Program != "sind" ||
				o.Err.Error() != `sind panicked; this is a bug, please report it: "assignment to entry in nil map"` {
				t.Errorf("exe2 = %+v, want no value and an error saying sind panicked", o)
			}
			continue
		}
		if o.Err != nil || o.Value == nil {
			t.Errorf("exe%d = %+v, want it to have finished", i+1, o)
		}
	}
	if !strings.HasPrefix(log.String(), `sind: panic while working on exe2: "assignment to entry in nil map"`) || !strings.Contains(log.String(), "goroutine") {
		t.Errorf("the log has no stack naming exe2:\n%s", log.String())
	}
}

// A pool reports a step with its total and limit, and every target queued
// before the first runs; the calls made for a target nest under it, and the
// step says which failed.
func TestMapReportsItsWork(t *testing.T) {
	t.Parallel()

	ctx, tree := progresstest.Watch(context.Background(), t)
	fanout.Map(ctx, nodes(5), fanout.Options[string]{Step: "reset the machines", Limit: 2, Describe: onBMC},
		func(ctx context.Context, node string) (struct{}, error) {
			_, call := progress.Start(ctx, progress.KindCall, "redfish", progress.HTTP("POST", "/redfish/v1/Systems/1"))
			var err error
			if node == "exe3" {
				err = unreachable{errors.New(node + ".mgmt.example.org: connection refused")}
			}
			call.End(err)
			return struct{}{}, err
		})
	// Without a Summarize of the program's, the step is the targets'
	// failure, whichever class they said.
	want := `step reset the machines total=5 limit=2 [fold]: failed (target): 1 of 5 failed: exe3
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

	ctx, tree := progresstest.Watch(context.Background(), t)
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

	ctx, tree := progresstest.Watch(context.Background(), t)
	ctx, cancel := context.WithCancel(ctx)
	fanout.Map(ctx, nodes(2), fanout.Options[string]{Step: "read the power state", Limit: 2},
		func(_ context.Context, node string) (struct{}, error) {
			if node == "exe2" {
				cancel()
				return struct{}{}, unreachable{errors.New("exe2: dial tcp: operation was canceled")}
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

// An item the work leaves out on purpose ends skipped with the reason,
// which its outcome carries; it counts towards the step's total, and is no
// failure of the step.
func TestMapEndsAnItemLeftOutSkipped(t *testing.T) {
	t.Parallel()

	ctx, tree := progresstest.Watch(context.Background(), t)
	outcomes := fanout.Map(ctx, nodes(3), fanout.Options[string]{Step: "write /etc/munge/munge.key", Limit: 2},
		func(_ context.Context, node string) (struct{}, error) {
			switch node {
			case "exe2":
				return struct{}{}, fanout.Skip("the node could not be reached")
			case "exe3":
				return struct{}{}, errors.New("exe3: command exited 1")
			}
			return struct{}{}, nil
		})
	if err := outcomes[1].Err; !fanout.IsSkipped(err) || err.Error() != "the node could not be reached" {
		t.Errorf("the outcome of exe2 is %+v, want it skipped with the reason", outcomes[1])
	}
	if fanout.IsSkipped(outcomes[2].Err) {
		t.Errorf("the failure of exe3 reads as skipped: %v", outcomes[2].Err)
	}
	want := `step write /etc/munge/munge.key total=3 limit=2 [fold]: failed (target): 1 of 3 failed: exe3
  target exe1: ok
  target exe2: skipped: the node could not be reached
  target exe3: failed (target): {}: command exited 1
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}
