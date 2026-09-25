// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progresstest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// pool runs n targets under a Fold step, limit at a time, the way the pool
// contract has it: every target queued before the first runs, each ended
// before its place is given up, and those never started ended as canceled
// once ctx ends. The targets in fail fail, and cancel, if set, is called
// once the target it names has started.
func pool(ctx context.Context, name string, n, limit int, fail map[int]bool, cancel map[int]context.CancelFunc) {
	stepCtx, step := progress.Start(ctx, progress.KindStep, name,
		progress.WithFlags(progress.Fold), progress.Total(n), progress.Limit(limit))
	ctxs, spans := make([]context.Context, n), make([]*progress.Span, n)
	for i := range n {
		node := fmt.Sprintf("exe%d", i+1)
		ctxs[i], spans[i] = progress.Start(stepCtx, progress.KindTarget, node, progress.Queued(),
			progress.Node(node), progress.Host(node+".hpc.example.org"))
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	failed := 0
	var mu sync.Mutex
	for i := range n {
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
			if ctx.Err() != nil {
				<-sem
			}
		}
		if ctx.Err() != nil {
			spans[i].End(ctx.Err())
			mu.Lock()
			failed++
			mu.Unlock()
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			spans[i].Run()
			if c := cancel[i]; c != nil {
				c()
			}
			time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond)
			_, call := progress.Start(ctxs[i], progress.KindCall, "ssh",
				progress.Host(fmt.Sprintf("exe%d.hpc.example.org", i+1)), progress.Timeout(time.Minute))
			var err error
			if fail[i] {
				err = fmt.Errorf("exe%d.hpc.example.org: command exited 1", i+1)
			}
			call.End(err, progress.Exit(map[bool]int{false: 0, true: 1}[fail[i]]))
			spans[i].End(err)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	var err error
	if failed > 0 {
		err = fmt.Errorf("%d of %d failed", failed, n)
	}
	step.End(err)
}

func watch(t *testing.T) (context.Context, *progress.Bus, *Capture) {
	t.Helper()
	c := &Capture{Lines: true}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	return progress.WithBus(context.Background(), bus), bus, c
}

func TestCheckAcceptsWorkThatKeepsThePromises(t *testing.T) {
	t.Parallel()

	ctx, bus, c := watch(t)
	ctx, cmd := progress.Start(ctx, progress.KindCommand, "provision reinstall")
	_, cred := progress.Start(ctx, progress.KindCall, "credential bmc", progress.WithFlags(progress.Hidden), progress.Source("prompt"))
	resume := progress.Suspend(ctx)
	resume()
	cred.End(nil)
	pool(ctx, "boot from the network once", 40, 8, map[int]bool{6: true, 30: true}, nil)

	// A staggered step: a batch that ran, one that failed, and one left
	// out, which counts as its Total.
	stepCtx, step := progress.Start(ctx, progress.KindStep, "power on", progress.WithFlags(progress.Fold), progress.Total(6), progress.Limit(2))
	var batches []*progress.Span
	var batchCtxs []context.Context
	for i := range 3 {
		bctx, b := progress.Start(stepCtx, progress.KindBatch, "batch", progress.Queued(), progress.Batch(i+1, 3), progress.Total(2), progress.Limit(2))
		batches, batchCtxs = append(batches, b), append(batchCtxs, bctx)
	}
	for i, b := range batches[:2] {
		b.Run()
		pool(batchCtxs[i], "redfish On", 2, 2, map[int]bool{1: i == 1}, nil)
		b.End(nil)
		_, wait := progress.Start(stepCtx, progress.KindWait, "stagger", progress.Timeout(5*time.Second))
		wait.End(nil)
	}
	batches[2].Skip("an earlier batch failed")
	step.End(errors.New("1 of 6 failed"))

	// Ctrl-C while a pool runs: every target still ends.
	cctx, cancel := context.WithCancel(ctx)
	pool(cctx, "reset the machines", 40, 4, nil, map[int]context.CancelFunc{9: cancel})
	cmd.End(context.Canceled)
	bus.Close()

	if problems := violations(c.Events()); len(problems) > 0 {
		t.Errorf("violations of work that keeps the promises:\n%s", strings.Join(problems, "\n"))
	}
}

// events builds events by hand, numbered in order: add appends one of
// type t about span, started under parent, and change adjusts it.
type events []progress.Event

func (es events) add(t progress.Type, span, parent progress.SpanID, k progress.Kind, change ...func(*progress.Event)) events {
	e := progress.Event{Seq: uint64(len(es) + 1), Type: t, Span: span, Parent: parent, Kind: k, Name: fmt.Sprintf("s%d", span)}
	switch t {
	case progress.TypeStart, progress.TypeRun:
		e.State = progress.StateRunning
	case progress.TypeEnd:
		e.State, e.Status = progress.StateEnded, progress.StatusOK
	}
	for _, f := range change {
		f(&e)
	}
	return append(es, e)
}

func queued(e *progress.Event) { e.State = progress.StateQueued }
func folds(total int) func(*progress.Event) {
	return func(e *progress.Event) { e.Flags |= progress.Fold; e.Total = total }
}

// Short names for the table below.
var (
	start = progress.TypeStart
	run   = progress.TypeRun
	end   = progress.TypeEnd
	step  = progress.KindStep
	tgt   = progress.KindTarget
	call  = progress.KindCall
)

func TestCheckFindsBrokenPromises(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events events
		want   string
	}{
		{"a gap in Seq", events{}.add(start, 1, 0, call).add(end, 1, 0, call, func(e *progress.Event) { e.Seq = 3 }), "event 2 has Seq 3"},
		{"a span id of zero", events{}.add(start, 0, 0, call).add(end, 0, 0, call), "id 0"},
		{"a span started twice", events{}.add(start, 1, 0, call).add(start, 1, 0, call).add(end, 1, 0, call), "a second time"},
		{"a span that never ends", events{}.add(start, 1, 0, call), "never ends"},
		{"an event after the end", events{}.add(start, 1, 0, call).add(end, 1, 0, call).add(end, 1, 0, call), "after call \"s1\" (event 1) ended"},
		{"an event before the start", events{}.add(run, 1, 0, call), "has not started"},
		{"an unknown parent", events{}.add(start, 2, 1, call).add(end, 2, 1, call), "which has not started"},
		{"a parent that has ended", events{}.add(start, 1, 0, step).add(end, 1, 0, step).add(start, 2, 1, call).add(end, 2, 1, call), "which has ended"},
		{"a parent that ends first", events{}.add(start, 1, 0, step).add(start, 2, 1, call).add(end, 1, 0, step).add(end, 2, 1, call), "ends before the 1 spans under it"},
		{"a run that was not queued", events{}.add(start, 1, 0, tgt).add(run, 1, 0, tgt).add(end, 1, 0, tgt), "which was not queued"},
		{"an end with no status", events{}.add(start, 1, 0, call).add(end, 1, 0, call, func(e *progress.Event) { e.Status = 0 }), "no status"},
		{"a target of a Fold step not queued",
			events{}.add(start, 1, 0, step, folds(1)).add(start, 2, 1, tgt).add(end, 2, 1, tgt).add(end, 1, 0, step, folds(1)),
			"is not queued under"},
		{"a target announced after one ran",
			events{}.add(start, 1, 0, step, folds(2)).add(start, 2, 1, tgt, queued).add(run, 2, 1, tgt).add(start, 3, 1, tgt, queued).
				add(end, 2, 1, tgt).add(end, 3, 1, tgt).add(end, 1, 0, step, folds(2)),
			"after another has run"},
		{"a Fold step that counts short of its Total",
			events{}.add(start, 1, 0, step, folds(2)).add(start, 2, 1, tgt, queued).add(end, 2, 1, tgt).add(end, 1, 0, step, folds(2)),
			"ends with 1 of its Total of 2"},
		{"a Fold step that counts past its Total",
			events{}.add(start, 1, 0, step, folds(1)).add(start, 2, 1, tgt, queued).add(start, 3, 1, tgt, queued).
				add(end, 2, 1, tgt).add(end, 3, 1, tgt).add(end, 1, 0, step, folds(1)),
			"more than its Total of 1"},
		{"a Total that shrinks",
			events{}.add(start, 1, 0, step, folds(0), func(e *progress.Event) { e.Total = 3 }).
				add(progress.TypeUpdate, 1, 0, step, folds(2)).add(end, 1, 0, step, folds(3)),
			"lowers the Total"},
		{"more targets at once than the Limit",
			events{}.add(start, 1, 0, step, func(e *progress.Event) { e.Limit = 1 }).add(start, 2, 1, tgt).add(start, 3, 1, tgt).
				add(end, 2, 1, tgt).add(end, 3, 1, tgt).add(end, 1, 0, step),
			"runs 2 targets at once, more than its Limit of 1"},
		{"a suspension never resumed", events{}.add(progress.TypeSuspend, 0, 0, 0), "never resumed"},
		{"a resume with nothing suspended", events{}.add(progress.TypeResume, 0, 0, 0), "resumes what was not suspended"},
		{"a flag that is not passed down",
			events{}.add(start, 1, 0, step, func(e *progress.Event) { e.Flags = progress.Hidden }).add(start, 2, 1, call).add(end, 2, 1, call).add(end, 1, 0, step),
			"lacks the hidden of its parent"},
		{"text that is not escaped", events{}.add(start, 1, 0, call).add(end, 1, 0, call, func(e *progress.Event) { e.Err = "\x1b[2J" }), "error that is not escaped"},
		{"text that is too long", events{}.add(start, 1, 0, call, func(e *progress.Event) { e.Message = strings.Repeat("x", 300) }).add(end, 1, 0, call),
			"message of 300 bytes"},
	}
	for _, tc := range tests {
		problems := strings.Join(violations(tc.events), "\n")
		if !strings.Contains(problems, tc.want) {
			t.Errorf("%s: violations %q, want one saying %q", tc.name, problems, tc.want)
		}
	}
}

func TestTreeDoesNotDependOnScheduling(t *testing.T) {
	t.Parallel()

	var trees []string
	for range 5 {
		ctx, bus, c := watch(t)
		ctx, cmd := progress.Start(ctx, progress.KindCommand, "exec")
		// Two pools at once, whose steps start in either order.
		var wg sync.WaitGroup
		wg.Go(func() { pool(ctx, "uptime", 12, 4, map[int]bool{2: true, 9: true}, nil) })
		wg.Go(func() { pool(ctx, "hostname", 3, 3, nil, nil) })
		wg.Wait()
		cmd.End(errors.New("2 of 12 failed"))
		bus.Close()
		Check(t, c.Events())
		trees = append(trees, c.Tree())
	}
	want := `command exec: failed (target): 2 of 12 failed
  step hostname total=3 limit=3 [fold]: ok
    target exe[1-3]: ok
      call ssh host={} timeout=1m0s exit=0: ok
  step uptime total=12 limit=4 [fold]: failed (target): 2 of 12 failed
    target exe[1-2,4-9,11-12]: ok
      call ssh host={} timeout=1m0s exit=0: ok
    target exe[3,10]: failed (target): {}: command exited 1
      call ssh host={} timeout=1m0s exit=1: failed (target): {}: command exited 1
`
	for i, tree := range trees {
		if tree != want {
			t.Errorf("run %d drew:\n%s\nwant:\n%s", i+1, tree, want)
		}
	}
}

func TestTreeDrawsWhatIsNotFinished(t *testing.T) {
	t.Parallel()

	ctx, bus, c := watch(t)
	ctx, _ = progress.Start(ctx, progress.KindCommand, "bmc power on")
	ctx, _ = progress.Start(ctx, progress.KindStep, "power on", progress.WithFlags(progress.Fold), progress.Total(20))
	for i := range 11 {
		_, b := progress.Start(ctx, progress.KindBatch, "batch", progress.Queued(), progress.Batch(i+1, 11), progress.Node(fmt.Sprintf("exe%d", i+1)))
		if i == 1 {
			b.Run()
		}
	}
	want := `command bmc power on: running
  step power on total=20 [fold]: running
    batch batch node=exe1 batch=1/11: queued
    batch batch node=exe2 batch=2/11: running
    batch batch node=exe3 batch=3/11: queued
    batch batch node=exe4 batch=4/11: queued
    batch batch node=exe5 batch=5/11: queued
    batch batch node=exe6 batch=6/11: queued
    batch batch node=exe7 batch=7/11: queued
    batch batch node=exe8 batch=8/11: queued
    batch batch node=exe9 batch=9/11: queued
    batch batch node=exe10 batch=10/11: queued
    batch batch node=exe11 batch=11/11: queued
`
	if got := c.Tree(); got != want {
		t.Errorf("drew:\n%s\nwant:\n%s", got, want)
	}
	bus.Close()
}

func TestFoldListsNamesThatAreNoNodeSet(t *testing.T) {
	t.Parallel()

	if got := fold([]string{"exe2", "exe1", "exe3"}); got != "exe[1-3]" {
		t.Errorf("fold of three nodes = %q", got)
	}
	if got := fold([]string{"port 10", "port 9"}); got != "port 9,port 10" {
		t.Errorf("fold of names no node set takes = %q", got)
	}
}
