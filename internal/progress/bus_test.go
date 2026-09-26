// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
)

// clock is a clock a test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// watched returns a context whose Bus sends to a capture, and closes the
// Bus and checks the events when the test ends.
func watched(t *testing.T, o progress.Options) (context.Context, *progress.Bus, *progresstest.Capture) {
	t.Helper()
	capture := &progresstest.Capture{}
	o.Sinks = append([]progress.Sink{capture}, o.Sinks...)
	bus := progress.NewBus(o)
	t.Cleanup(func() {
		bus.Close()
		progresstest.Check(t, capture.Events())
	})
	return progress.WithBus(context.Background(), bus), bus, capture
}

// types lists what the events report, as "type kind name".
func types(events []progress.Event) []string {
	var out []string
	for _, e := range events {
		s := e.Type.String()
		if e.Kind != 0 {
			s += " " + e.Kind.String() + " " + e.Name
		}
		out = append(out, s)
	}
	return out
}

func TestWithoutABusNothingIsReported(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	got, span := progress.Start(ctx, progress.KindCommand, "exec", progress.Total(3))
	if got != ctx || span != nil {
		t.Fatalf("Start without a Bus = %v, %v; want the context itself and a nil span", got, span)
	}
	// Every method of a nil span does nothing, rather than panic.
	span.Run()
	span.Update(progress.Total(4))
	span.End(errors.New("failed"))
	span.Skip("dry run")
	progress.Suspend(ctx)()
	if progress.SpanFrom(ctx) != nil || progress.BusFrom(ctx) != nil {
		t.Error("a context without a Bus carries a span or a Bus")
	}
	var buf bytes.Buffer
	if w := progress.Tee(ctx, &buf, progress.Stdout, nil); w != &buf {
		t.Errorf("Tee without a Bus or a parser = %T, want the writer itself", w)
	}
	if ctx := progress.WithBus(ctx, nil); progress.BusFrom(ctx) != nil {
		t.Error("WithBus of nil put a Bus in the context")
	}
}

// TestWithoutABusNothingIsAllocated is the promise that makes it cheap to
// report work nobody watches: a command without a display builds no Bus.
func TestWithoutABusNothingIsAllocated(t *testing.T) {
	ctx := context.Background()
	allocs := testing.AllocsPerRun(100, func() { lifecycle(ctx) })
	if allocs != 0 {
		t.Errorf("a target's lifecycle without a Bus allocates %v times, want 0", allocs)
	}
}

// lifecycle is what a pool and a traced call report for one target.
func lifecycle(ctx context.Context) {
	ctx, target := progress.Start(ctx, progress.KindTarget, "exe0001", progress.Queued(),
		progress.Node("exe0001"), progress.Host("exe0001.hpc.example.org"), progress.Role("compute"))
	target.Run()
	_, call := progress.Start(ctx, progress.KindCall, "ssh", progress.Timeout(10*time.Minute))
	call.End(nil, progress.Exit(0))
	target.End(nil)
}

func BenchmarkTargetLifecycle(b *testing.B) {
	b.Run("without a bus", func(b *testing.B) {
		ctx := context.Background()
		for b.Loop() {
			lifecycle(ctx)
		}
	})
	b.Run("with a bus", func(b *testing.B) {
		bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{discard{}}})
		defer bus.Close()
		ctx, step := progress.Start(progress.WithBus(context.Background(), bus), progress.KindStep, "uptime")
		defer step.End(nil)
		for b.Loop() {
			lifecycle(ctx)
		}
	})
}

type discard struct{}

func (discard) Handle(progress.Event) {}

func TestASpanReportsItsLifeInOrder(t *testing.T) {
	t.Parallel()

	clock := newClock()
	ctx, bus, capture := watched(t, progress.Options{Now: clock.Now})
	cmdCtx, cmd := progress.Start(ctx, progress.KindCommand, "bmc power on", progress.WithFlags(progress.DryRun))
	stepCtx, step := progress.Start(cmdCtx, progress.KindStep, "power on", progress.WithFlags(progress.Fold), progress.Total(1), progress.Limit(8))
	_, target := progress.Start(stepCtx, progress.KindTarget, "exe0001", progress.Queued(), progress.Node("exe0001"))
	clock.Add(time.Second)
	target.Run()
	step.Update(progress.Message("1 to go"))
	target.End(nil, progress.HTTPStatus(204))
	step.End(nil)
	cmd.End(nil)
	bus.Close()

	events := capture.Events()
	want := []string{
		"start command bmc power on", "start step power on", "start target exe0001",
		"run target exe0001", "update step power on", "end target exe0001",
		"end step power on", "end command bmc power on",
	}
	if got := types(events); !equal(got, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Errorf("event %d has Seq %d", i+1, e.Seq)
		}
	}
	if events[1].Parent != events[0].Span || events[2].Parent != events[1].Span || events[0].Parent != 0 {
		t.Errorf("the parents do not follow the contexts: %v", events)
	}
	if e := events[2]; e.State != progress.StateQueued || e.Node != "exe0001" {
		t.Errorf("the target starts %s with Node %q, want queued exe0001", e.State, e.Node)
	}
	if e := events[3]; e.State != progress.StateRunning || !e.Time.Equal(clock.Now()) {
		t.Errorf("Run reports %s at %v, want running at %v", e.State, e.Time, clock.Now())
	}
	if e := events[4]; e.Message != "1 to go" || e.Total != 1 || e.Limit != 8 {
		t.Errorf("Update reports %+v", e.Fields)
	}
	if e := events[5]; e.Status != progress.StatusOK || e.HTTPStatus != 204 || e.State != progress.StateEnded {
		t.Errorf("End reports %s, HTTP %d, %s", e.Status, e.HTTPStatus, e.State)
	}
	if events[0].Flags != progress.DryRun || events[1].Flags != progress.Fold {
		t.Errorf("flags %s and %s, want dry-run on the command only and fold on the step", events[0].Flags, events[1].Flags)
	}
}

func TestOnlyTheFirstEndCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		finish func(*progress.Span)
		want   progress.Status
	}{
		{"End after End", func(s *progress.Span) { s.End(nil); s.End(errors.New("late")) }, progress.StatusOK},
		{"Skip after End", func(s *progress.Span) { s.End(errors.New("refused")); s.Skip("dry run") }, progress.StatusFailed},
		{"End after Skip", func(s *progress.Span) { s.Skip("dry run"); s.End(nil) }, progress.StatusSkipped},
		{"Run and Update after End", func(s *progress.Span) { s.End(nil); s.Run(); s.Update(progress.Total(9)) }, progress.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, bus, capture := watched(t, progress.Options{})
			_, span := progress.Start(ctx, progress.KindCall, "ssh", progress.Queued())
			tc.finish(span)
			bus.Close()
			var ends []progress.Event
			for _, e := range capture.Events() {
				if e.Type != progress.TypeStart {
					ends = append(ends, e)
				}
			}
			if len(ends) != 1 || ends[0].Type != progress.TypeEnd || ends[0].Status != tc.want {
				t.Errorf("events after the start: %v, want one End %s", types(ends), tc.want)
			}
		})
	}
}

func TestEndTellsTheOutcomeFromTheError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		status progress.Status
		class  progress.Class
		text   string
	}{
		{"no error is ok", nil, progress.StatusOK, progress.ClassNone, ""},
		{"an error is a failure", errors.New("exe0001: command exited 1"), progress.StatusFailed, progress.ClassTarget, "exe0001: command exited 1"},
		{"an interrupt is canceled", fmt.Errorf("exe0001: %w", context.Canceled), progress.StatusCanceled, progress.ClassCanceled, "exe0001: context canceled"},
		{"a deadline is a failure", context.DeadlineExceeded, progress.StatusFailed, progress.ClassTimeout, "context deadline exceeded"},
		{"the text is one escaped line", errors.New("no\nanswer \x1b[2J"), progress.StatusFailed, progress.ClassTarget, `no\nanswer \x1b[2J`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, bus, capture := watched(t, progress.Options{})
			_, span := progress.Start(ctx, progress.KindCall, "ssh")
			span.End(tc.err)
			bus.Close()
			e := capture.Events()[1]
			if e.Status != tc.status || e.Class != tc.class || e.Err != tc.text {
				t.Errorf("End(%v) = %s, %s, %q; want %s, %s, %q", tc.err, e.Status, e.Class, e.Err, tc.status, tc.class, tc.text)
			}
		})
	}

	t.Run("a long error is cut", func(t *testing.T) {
		t.Parallel()
		ctx, bus, capture := watched(t, progress.Options{})
		_, span := progress.Start(ctx, progress.KindCall, "ipmi")
		span.End(errors.New(strings.Repeat("é", progress.MaxErr)))
		bus.Close()
		if e := capture.Events()[1]; len(e.Err) > progress.MaxErr || !strings.HasPrefix(e.Err, "éé") {
			t.Errorf("an error of %d bytes ends with an Err of %d bytes", 2*progress.MaxErr, len(e.Err))
		}
	})
}

func TestSkipIsTheEndOfASpanLeftOut(t *testing.T) {
	t.Parallel()

	ctx, bus, capture := watched(t, progress.Options{})
	stepCtx, step := progress.Start(ctx, progress.KindStep, "power on", progress.WithFlags(progress.Fold), progress.Total(4))
	firstCtx, first := progress.Start(stepCtx, progress.KindBatch, "batch", progress.Queued(), progress.Batch(1, 2), progress.Total(2))
	_, second := progress.Start(stepCtx, progress.KindBatch, "batch", progress.Queued(), progress.Batch(2, 2), progress.Total(2))
	first.Run()
	for _, node := range []string{"exe1", "exe2"} {
		_, target := progress.Start(firstCtx, progress.KindTarget, node, progress.Queued())
		target.End(errors.New("refused"))
	}
	first.End(errors.New("2 of 2 failed"))
	second.Skip("an earlier batch failed")
	step.End(errors.New("2 of 4 failed"))
	bus.Close()

	e := capture.Events()[len(capture.Events())-2]
	if e.Type != progress.TypeEnd || e.Status != progress.StatusSkipped || e.Class != progress.ClassNone ||
		e.Err != "an earlier batch failed" || e.Batch != "2/2" {
		t.Errorf("Skip reported %s %s %s %q batch %s", e.Type, e.Status, e.Class, e.Err, e.Batch)
	}
	// The count reaches the step's Total although the batch never ran,
	// which Check, run at cleanup, confirms.
}

func TestHiddenAndShowLinesArePassedDown(t *testing.T) {
	t.Parallel()

	ctx, bus, capture := watched(t, progress.Options{})
	ctx, cmd := progress.Start(ctx, progress.KindCommand, "exec", progress.WithFlags(progress.DryRun))
	ctx, step := progress.Start(ctx, progress.KindStep, "uptime", progress.WithFlags(progress.Fold|progress.ShowLines))
	ctx, lookup := progress.Start(ctx, progress.KindCall, "credential bmc", progress.WithFlags(progress.Hidden))
	_, inner := progress.Start(ctx, progress.KindCall, "ssh")
	inner.End(nil)
	lookup.End(nil)
	step.End(nil)
	cmd.End(nil)
	bus.Close()

	want := []progress.Flags{progress.DryRun, progress.Fold | progress.ShowLines, progress.Hidden | progress.ShowLines, progress.Hidden | progress.ShowLines}
	for i, e := range capture.Events()[:4] {
		if e.Flags != want[i] {
			t.Errorf("%s %s has flags %s, want %s", e.Kind, e.Name, e.Flags, want[i])
		}
	}
}

func TestTotalNeverShrinks(t *testing.T) {
	t.Parallel()

	ctx, bus, capture := watched(t, progress.Options{})
	_, span := progress.Start(ctx, progress.KindStep, "ssh", progress.Total(5))
	span.Update(progress.Total(3), progress.Node("ignored"))
	span.Update(progress.Total(8))
	span.End(nil, progress.Total(2))
	bus.Close()

	var totals []int
	for _, e := range capture.Events() {
		totals = append(totals, e.Total)
		if e.Node != "" {
			t.Errorf("Update set Node %q; it sets only Total and Message", e.Node)
		}
	}
	if fmt.Sprint(totals) != "[5 5 8 8]" {
		t.Errorf("Totals %v, want [5 5 8 8]", totals)
	}
}

func TestAParentEndsAfterItsChildren(t *testing.T) {
	t.Parallel()

	ctx, bus, capture := watched(t, progress.Options{})
	stepCtx, step := progress.Start(ctx, progress.KindStep, "copy")
	targetCtx, target := progress.Start(stepCtx, progress.KindTarget, "exe0001")
	_, call := progress.Start(targetCtx, progress.KindCall, "scp")
	step.End(nil)
	call.End(nil) // too late: it has been ended with its parent
	target.End(errors.New("too late"))
	bus.Close()

	events := capture.Events()
	want := []string{"start step copy", "start target exe0001", "start call scp", "end call scp", "end target exe0001", "end step copy"}
	if got := types(events); !equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for _, e := range events[3:5] {
		if e.Status != progress.StatusCanceled || e.Err != "not finished" {
			t.Errorf("%s %s ended %s %q, want canceled, not finished", e.Kind, e.Name, e.Status, e.Err)
		}
	}
}

// A span started under one that has ended, by a worker whose target was
// ended for it, is not started: it would run on under a span that is over,
// and end after the step above it.
func TestNothingStartsUnderASpanThatEnded(t *testing.T) {
	t.Parallel()

	ctx, bus, capture := watched(t, progress.Options{})
	stepCtx, step := progress.Start(ctx, progress.KindStep, "copy")
	targetCtx, target := progress.Start(stepCtx, progress.KindTarget, "exe0001")
	target.End(nil)
	later, call := progress.Start(targetCtx, progress.KindCall, "scp")
	if later != targetCtx || call != nil {
		t.Errorf("Start under an ended target returned %v and %v, want its context and no span", later, call)
	}
	call.End(nil)
	step.End(nil)
	bus.Close()

	want := []string{"start step copy", "start target exe0001", "end target exe0001", "end step copy"}
	if got := types(capture.Events()); !equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
	progresstest.Check(t, capture.Events())
}

func TestCloseEndsWhatIsStillOpen(t *testing.T) {
	t.Parallel()

	ctx, bus, capture := watched(t, progress.Options{})
	ctx, cmd := progress.Start(ctx, progress.KindCommand, "exec")
	ctx, step := progress.Start(ctx, progress.KindStep, "uptime")
	_, target := progress.Start(ctx, progress.KindTarget, "exe0001", progress.Queued())
	bus.Close()
	bus.Close()

	events := capture.Events()
	want := []string{"start command exec", "start step uptime", "start target exe0001", "end target exe0001", "end step uptime", "end command exec"}
	if got := types(events); !equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for _, e := range events[3:] {
		if e.Status != progress.StatusCanceled || e.Class != progress.ClassCanceled || e.Err != "not finished" {
			t.Errorf("%s %s ended %s %s %q", e.Kind, e.Name, e.Status, e.Class, e.Err)
		}
	}

	// Nothing is reported once the Bus is closed.
	target.End(nil)
	step.Update(progress.Total(1))
	later, span := progress.Start(ctx, progress.KindCall, "ssh")
	if later != ctx || span != nil {
		t.Error("Start after Close returned a span")
	}
	progress.Suspend(ctx)()
	if n := len(capture.Events()); n != len(events) {
		t.Errorf("%d events after Close", n-len(events))
	}
	_ = cmd
}

func TestSpanIDsAreNeverZeroAndNeverShared(t *testing.T) {
	t.Parallel()

	// Runs that adopt one trace, as several would from one TRACEPARENT,
	// share a trace id; their span ids must still differ.
	trace := progress.TraceID{1, 2, 3}
	seen := map[progress.SpanID]bool{}
	for range 2 {
		capture := &progresstest.Capture{}
		bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{capture}, Trace: trace})
		if bus.Trace() != trace {
			t.Errorf("Trace() = %s, want %s", bus.Trace(), trace)
		}
		ctx := progress.WithBus(context.Background(), bus)
		for range 1000 {
			_, span := progress.Start(ctx, progress.KindCall, "ssh")
			span.End(nil)
		}
		bus.Close()
		for _, e := range capture.Events() {
			if e.Type != progress.TypeStart {
				continue
			}
			if e.Span == 0 || seen[e.Span] {
				t.Fatalf("span id %s is zero or used twice", e.Span)
			}
			seen[e.Span] = true
		}
	}

	a, b := progress.NewBus(progress.Options{}), progress.NewBus(progress.Options{})
	if a.Trace() == (progress.TraceID{}) || a.Trace() == b.Trace() {
		t.Errorf("drawn traces %s and %s", a.Trace(), b.Trace())
	}
	if s := a.Trace().String(); len(s) != 32 {
		t.Errorf("a trace id reads %q, want 32 hexadecimal digits", s)
	}
	if s := progress.SpanID(0xabc).String(); s != "0000000000000abc" {
		t.Errorf("a span id reads %q", s)
	}
}

func TestASpanOfAnotherBusIsNoParent(t *testing.T) {
	t.Parallel()

	outer, _, _ := watched(t, progress.Options{})
	outer, _ = progress.Start(outer, progress.KindCommand, "mcp serve")
	inner, bus, capture := watched(t, progress.Options{})
	ctx := progress.WithBus(outer, bus)
	if progress.SpanFrom(ctx) != nil {
		t.Error("SpanFrom returned the span of another Bus")
	}
	_, span := progress.Start(ctx, progress.KindCommand, "exec")
	span.End(nil)
	if e := capture.Events()[0]; e.Parent != 0 {
		t.Errorf("the command starts under span %s of another Bus", e.Parent)
	}
	_ = inner
}

func TestEventsHaveOneOrderUnderConcurrency(t *testing.T) {
	t.Parallel()

	ctx, bus, capture := watched(t, progress.Options{})
	stepCtx, step := progress.Start(ctx, progress.KindStep, "uptime", progress.WithFlags(progress.Fold), progress.Total(64), progress.Limit(8))
	spans := make([]*progress.Span, 64)
	ctxs := make([]context.Context, 64)
	for i := range spans {
		ctxs[i], spans[i] = progress.Start(stepCtx, progress.KindTarget, fmt.Sprintf("exe%d", i), progress.Queued())
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range spans {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			spans[i].Run()
			_, call := progress.Start(ctxs[i], progress.KindCall, "ssh")
			call.End(nil)
			spans[i].End(nil)
		})
	}
	wg.Wait()
	step.End(nil)
	bus.Close()

	for i, e := range capture.Events() {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has Seq %d", i+1, e.Seq)
		}
	}
}

// panicky panics on its nth event, or when asked for lines or told the
// trace.
type panicky struct {
	n, seen      int
	lines, begin bool
}

func (p *panicky) Begin(progress.TraceContext) {
	if p.begin {
		panic("the log could not begin")
	}
}

func (p *panicky) Handle(progress.Event) {
	p.seen++
	if p.seen == p.n {
		panic("drawing went wrong")
	}
}

func (p *panicky) WantsLines() bool {
	if p.lines {
		panic("asked for lines")
	}
	return true
}

func TestASinkThatPanicsIsRemoved(t *testing.T) {
	t.Parallel()

	var log bytes.Buffer
	bad := &panicky{n: 2}
	ctx, bus, capture := watched(t, progress.Options{Sinks: []progress.Sink{bad}, PanicLog: &log})
	for range 3 {
		_, span := progress.Start(ctx, progress.KindCall, "ssh")
		span.End(nil)
	}
	bus.Close()

	if bad.seen != 2 {
		t.Errorf("the sink that panicked was sent %d events, want 2", bad.seen)
	}
	if n := len(capture.Events()); n != 6 {
		t.Errorf("the other sink was sent %d events, want 6", n)
	}
	if !strings.Contains(log.String(), `panicked and was stopped: "drawing went wrong"`) || !strings.Contains(log.String(), "goroutine") {
		t.Errorf("the panic log reads %q, want the panic and its stack", log.String())
	}

	// A sink that panics when asked for lines asks for none.
	lines := progress.NewBus(progress.Options{Sinks: []progress.Sink{&panicky{lines: true}}, PanicLog: io.Discard})
	lctx, span := progress.Start(progress.WithBus(context.Background(), lines), progress.KindCall, "ssh", progress.WithFlags(progress.ShowLines))
	var buf bytes.Buffer
	if w := progress.Tee(lctx, &buf, progress.Stdout, nil); w != &buf {
		t.Errorf("Tee = %T, want the writer itself", w)
	}
	span.End(nil)
	lines.Close()

	// A sink that panics when told the trace is sent nothing.
	begins := &panicky{begin: true}
	traced := progress.NewBus(progress.Options{Sinks: []progress.Sink{begins}, PanicLog: io.Discard})
	_, span = progress.Start(progress.WithBus(context.Background(), traced), progress.KindCall, "ssh")
	span.End(nil)
	traced.Close()
	if begins.seen != 0 {
		t.Errorf("the sink that panicked when told the trace was sent %d events", begins.seen)
	}
}

// terminal is a Suspender that records its calls, and checks that the Bus
// is not locked while it is called.
type terminal struct {
	t     *testing.T
	ctx   context.Context
	mu    sync.Mutex
	calls []string
	panic bool
}

func (d *terminal) Handle(progress.Event) {}

func (d *terminal) Suspend() {
	d.record("suspend")
	if d.panic {
		panic("the terminal went away")
	}
	d.unlocked()
}

func (d *terminal) Resume() { d.record("resume") }

func (d *terminal) record(call string) {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
}

// unlocked fails unless another goroutine can report while the display is
// being suspended, as it could not if the Bus lock were held.
func (d *terminal) unlocked() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, span := progress.Start(d.ctx, progress.KindCall, "concurrent")
		span.End(nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		d.t.Error("the Bus stayed locked while a display was suspended")
	}
}

func (d *terminal) Calls() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.calls, " ")
}

func TestSuspendReturnsOnceTheDisplaysAreOff(t *testing.T) {
	t.Parallel()

	display := &terminal{t: t}
	ctx, bus, capture := watched(t, progress.Options{Sinks: []progress.Sink{display}})
	display.ctx = ctx
	gateCtx, gate := progress.Start(ctx, progress.KindWait, "confirm")

	resume := progress.Suspend(gateCtx)
	if got := display.Calls(); got != "suspend" {
		t.Fatalf("after Suspend the display saw %q, want suspend", got)
	}
	inner := progress.Suspend(gateCtx) // a password prompt inside the question
	inner()
	inner()
	resume()
	resume()
	gate.End(nil)
	if got := display.Calls(); got != "suspend suspend resume resume" {
		t.Errorf("the display saw %q", got)
	}

	var got []string
	for _, e := range capture.Events() {
		if e.Type == progress.TypeSuspend || e.Type == progress.TypeResume {
			got = append(got, e.Type.String())
			if e.Span == 0 {
				t.Errorf("%s names no span", e.Type)
			}
		}
	}
	if strings.Join(got, " ") != "suspend suspend resume resume" {
		t.Errorf("events %v", got)
	}

	// Close puts back a display still suspended, once.
	left := progress.Suspend(ctx)
	bus.Close()
	left()
	if got := display.Calls(); !strings.HasSuffix(got, "suspend resume") || strings.Count(got, "resume") != 3 {
		t.Errorf("after Close the display saw %q", got)
	}
}

func TestADisplayThatPanicsWhenSuspendedIsRemoved(t *testing.T) {
	t.Parallel()

	display := &terminal{t: t, panic: true}
	ctx, bus, _ := watched(t, progress.Options{Sinks: []progress.Sink{display}, PanicLog: io.Discard})
	display.ctx = ctx
	progress.Suspend(ctx)()
	progress.Suspend(ctx)()
	bus.Close()
	if got := display.Calls(); got != "suspend" {
		t.Errorf("the display saw %q, want one suspend and then nothing", got)
	}
}

func equal(a, b []string) bool {
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}
