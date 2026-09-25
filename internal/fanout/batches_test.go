// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// set parses a node set, or fails the test.
func set(t *testing.T, expr string) *nodeset.NodeSet {
	t.Helper()
	ns, err := nodeset.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

// sender stands in for the work of a batch: it reports each node of the
// batch as a target, queued before the first runs, the way a pool does,
// and fails the nodes in fail. Every call is written to log.
type sender struct {
	log  []string
	fail map[string]bool
}

func (s *sender) run(ctx context.Context, batch *nodeset.NodeSet) error {
	s.log = append(s.log, "run "+batch.String())
	names := batch.Expand()
	spans := make([]*progress.Span, len(names))
	for i, node := range names {
		_, spans[i] = progress.Start(ctx, progress.KindTarget, node, progress.Queued(), progress.Node(node))
	}
	var failed error
	for i, node := range names {
		spans[i].Run()
		var err error
		if s.fail[node] {
			err = fmt.Errorf("%s: no answer", node)
			failed = err
		}
		spans[i].End(err)
	}
	return failed
}

// options returns batch options whose hooks and pauses write to the
// sender's log, and whose pauses end at once.
func (s *sender) options(size int, pause time.Duration) fanout.BatchOptions {
	return fanout.BatchOptions{
		Step:  "power on",
		Size:  size,
		Limit: 2,
		Pause: pause,
		After: func(d time.Duration) <-chan time.Time {
			s.log = append(s.log, fmt.Sprintf("wait %s", d))
			ready := make(chan time.Time, 1)
			ready <- time.Time{}
			return ready
		},
		BeforePause: func(d time.Duration) { s.log = append(s.log, fmt.Sprintf("note: waiting %s", d)) },
		Before: func(i, n int, batch *nodeset.NodeSet) {
			s.log = append(s.log, fmt.Sprintf("note: %s (%d of %d)", batch, i+1, n))
		},
	}
}

// The set is split into as few batches as the size allows, evenly, in
// node set order, the way nodeset.Split splits it: ten at 8 go as 5 and 5.
func TestBatchesSplitTheSetEvenly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  string
		size int
		want string
	}{
		{"ten at eight", "exe[1-10]", 8, "exe[1-5] exe[6-10]"},
		{"ten at three", "exe[1-10]", 3, "exe[1-3] exe[4-6] exe[7-8] exe[9-10]"},
		{"fewer than a batch", "exe[1-3]", 8, "exe[1-3]"},
		{"exactly a batch", "exe[1-8]", 8, "exe[1-8]"},
		{"no size", "exe[1-10]", 0, "exe[1-10]"},
		{"the largest size", "exe[1-10]", math.MaxInt, "exe[1-10]"},
		{"nothing to send", "", 8, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sender{}
			batches := fanout.Batches(context.Background(), set(t, tc.set), s.options(tc.size, 0), s.run)
			var got []string
			for _, b := range batches {
				if !b.Ran || b.Err != nil {
					t.Errorf("batch %s = %+v, want it run without an error", b.Nodes, b)
				}
				got = append(got, b.Nodes.String())
			}
			if strings.Join(got, " ") != tc.want {
				t.Errorf("batches %v, want %s", got, tc.want)
			}
		})
	}
}

// A batch is sent once the one before has returned and the pause has
// passed, each pause announced before it starts and each batch before it
// is sent; with no pause, nothing waits and nothing is announced.
func TestBatchesPauseBetweenBatches(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		pause time.Duration
		want  []string
	}{
		{"a pause", 5 * time.Second, []string{
			"note: exe[1-2] (1 of 3)", "run exe[1-2]",
			"note: waiting 5s", "wait 5s",
			"note: exe[3-4] (2 of 3)", "run exe[3-4]",
			"note: waiting 5s", "wait 5s",
			"note: exe[5-6] (3 of 3)", "run exe[5-6]",
		}},
		{"no pause", 0, []string{
			"note: exe[1-2] (1 of 3)", "run exe[1-2]",
			"note: exe[3-4] (2 of 3)", "run exe[3-4]",
			"note: exe[5-6] (3 of 3)", "run exe[5-6]",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sender{}
			fanout.Batches(context.Background(), set(t, "exe[1-6]"), s.options(2, tc.pause), s.run)
			if got, want := strings.Join(s.log, "\n"), strings.Join(tc.want, "\n"); got != want {
				t.Errorf("got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// A batch with a failure may be the breaker the batches are there to
// protect, so the batches after it are not tried, and there is no pause
// after it either.
func TestBatchesStopAfterAFailedBatch(t *testing.T) {
	t.Parallel()

	s := &sender{fail: map[string]bool{"exe4": true}}
	batches := fanout.Batches(context.Background(), set(t, "exe[1-8]"), s.options(2, time.Second), s.run)
	want := []string{
		"note: exe[1-2] (1 of 4)", "run exe[1-2]",
		"note: waiting 1s", "wait 1s",
		"note: exe[3-4] (2 of 4)", "run exe[3-4]",
	}
	if got := strings.Join(s.log, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	for i, b := range batches {
		switch {
		case i == 0 && (!b.Ran || b.Err != nil):
			t.Errorf("batch 1 = %+v, want it run without an error", b)
		case i == 1 && (!b.Ran || b.Err == nil || !strings.Contains(b.Err.Error(), "exe4")):
			t.Errorf("batch 2 = %+v, want it run with exe4's error", b)
		case i > 1 && (b.Ran || !errors.Is(b.Err, fanout.ErrNotTried)):
			t.Errorf("batch %d = %+v, want it not tried", i+1, b)
		}
	}
}

// An interrupt during a pause, or between batches without one, sends
// nothing more: the batches left are reported as the context ended them.
// The batch under way when it came has been run.
func TestBatchesStopWhenInterrupted(t *testing.T) {
	t.Parallel()

	t.Run("during a pause", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		s := &sender{}
		o := s.options(2, time.Minute)
		o.After = func(time.Duration) <-chan time.Time {
			s.log = append(s.log, "interrupted while waiting")
			cancel()
			return nil
		}
		batches := fanout.Batches(ctx, set(t, "exe[1-6]"), o, s.run)
		want := "note: exe[1-2] (1 of 3)\nrun exe[1-2]\nnote: waiting 1m0s\ninterrupted while waiting"
		if got := strings.Join(s.log, "\n"); got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
		for i, b := range batches[1:] {
			if b.Ran || !errors.Is(b.Err, context.Canceled) {
				t.Errorf("batch %d = %+v, want it left out as cancelled", i+2, b)
			}
		}
	})

	t.Run("during a batch, without a pause", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		s := &sender{}
		batches := fanout.Batches(ctx, set(t, "exe[1-6]"), s.options(2, 0), func(ctx context.Context, batch *nodeset.NodeSet) error {
			cancel()
			return s.run(ctx, batch)
		})
		if got, want := strings.Join(s.log, "\n"), "note: exe[1-2] (1 of 3)\nrun exe[1-2]"; got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
		if !batches[0].Ran {
			t.Errorf("batch 1 = %+v, want it run", batches[0])
		}
		for i, b := range batches[1:] {
			if b.Ran || !errors.Is(b.Err, context.Canceled) {
				t.Errorf("batch %d = %+v, want it left out as cancelled", i+2, b)
			}
		}
	})
}

// The batches are reported under one step whose total is the whole set,
// all queued up front, with a wait for each pause; a batch not tried is
// skipped, and one an interrupt left out is canceled, and both count as
// their total.
func TestBatchesReportTheirWork(t *testing.T) {
	t.Parallel()

	t.Run("a failed batch", func(t *testing.T) {
		t.Parallel()
		ctx, tree := watch(t)
		s := &sender{fail: map[string]bool{"exe5": true}}
		fanout.Batches(ctx, set(t, "exe[1-7]"), s.options(3, 5*time.Second), s.run)
		want := `step power on total=7 [fold]: failed (target): exe5: no answer
  batch 1/3 node=exe[1-3] batch=1/3 total=3 limit=2: ok
    target exe[1-3]: ok
  batch 2/3 node=exe[4-5] batch=2/3 total=2 limit=2: failed (target): exe5: no answer
    target exe4: ok
    target exe5: failed (target): {}: no answer
  batch 3/3 node=exe[6-7] batch=3/3 total=2 limit=2: skipped: not tried: an earlier batch failed
  wait stagger timeout=5s: ok
`
		if got := tree(); got != want {
			t.Errorf("tree:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("an interrupt during a pause", func(t *testing.T) {
		t.Parallel()
		ctx, tree := watch(t)
		ctx, cancel := context.WithCancel(ctx)
		s := &sender{}
		o := s.options(2, 5*time.Second)
		o.After = func(time.Duration) <-chan time.Time { cancel(); return nil }
		fanout.Batches(ctx, set(t, "exe[1-6]"), o, s.run)
		want := `step power on total=6 [fold]: canceled (canceled): context canceled
  batch 1/3 node=exe[1-2] batch=1/3 total=2 limit=2: ok
    target exe[1-2]: ok
  batch 2/3 node=exe[3-4] batch=2/3 total=2 limit=2: canceled (canceled): context canceled
  batch 3/3 node=exe[5-6] batch=3/3 total=2 limit=2: canceled (canceled): context canceled
  wait stagger timeout=5s: canceled (canceled): context canceled
`
		if got := tree(); got != want {
			t.Errorf("tree:\n%s\nwant:\n%s", got, want)
		}
	})

	// A batch the interrupt cut short fails, but the batches after it were
	// left out for the interrupt, not for that failure.
	t.Run("an interrupt during a batch", func(t *testing.T) {
		t.Parallel()
		ctx, tree := watch(t)
		ctx, cancel := context.WithCancel(ctx)
		s := &sender{fail: map[string]bool{"exe4": true}}
		run := func(ctx context.Context, batch *nodeset.NodeSet) error {
			if batch.Contains("exe4") {
				cancel()
			}
			return s.run(ctx, batch)
		}
		batches := fanout.Batches(ctx, set(t, "exe[1-6]"), s.options(2, 5*time.Second), run)
		if !errors.Is(batches[2].Err, context.Canceled) {
			t.Errorf("the batch left out ended %v, want the interrupt", batches[2].Err)
		}
		want := `step power on total=6 [fold]: failed (target): exe4: no answer
  batch 1/3 node=exe[1-2] batch=1/3 total=2 limit=2: ok
    target exe[1-2]: ok
  batch 2/3 node=exe[3-4] batch=2/3 total=2 limit=2: failed (target): exe4: no answer
    target exe3: ok
    target exe4: failed (target): {}: no answer
  batch 3/3 node=exe[5-6] batch=3/3 total=2 limit=2: canceled (canceled): context canceled
  wait stagger timeout=5s: ok
`
		if got := tree(); got != want {
			t.Errorf("tree:\n%s\nwant:\n%s", got, want)
		}
	})

	// A context that runs out of time leaves the batches out as an
	// interrupt does: canceled, counted as their Total, not failed.
	t.Run("a deadline during a pause", func(t *testing.T) {
		t.Parallel()
		ctx, tree := watch(t)
		ctx, cancel := context.WithDeadline(ctx, time.Now().Add(time.Hour))
		defer cancel()
		deadline, stop := context.WithTimeout(ctx, 0)
		defer stop()
		s := &sender{}
		o := s.options(2, 5*time.Second)
		o.After = func(time.Duration) <-chan time.Time { return nil }
		fanout.Batches(deadline, set(t, "exe[1-4]"), o, s.run)
		want := `step power on total=4 [fold]: failed (timeout): context deadline exceeded
  batch 1/2 node=exe[1-2] batch=1/2 total=2 limit=2: ok
    target exe[1-2]: ok
  batch 2/2 node=exe[3-4] batch=2/2 total=2 limit=2: canceled (canceled): context deadline exceeded
  wait stagger timeout=5s: failed (timeout): context deadline exceeded
`
		if got := tree(); got != want {
			t.Errorf("tree:\n%s\nwant:\n%s", got, want)
		}
	})
}
