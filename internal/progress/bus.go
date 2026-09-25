// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"slices"
	"sync"
	"time"
)

// Options configure a Bus.
type Options struct {
	// Sinks receive every event, in the order given.
	Sinks []Sink
	// Now is the clock the events are stamped with; nil is time.Now.
	Now func() time.Time
	// Trace is the trace the spans belong to; zero draws one at random.
	Trace TraceID
	// PanicLog receives the stack of a sink that panicked, the front
	// end's diagnostics; nil is the process's standard error.
	PanicLog io.Writer
}

// Bus hands the events of one command's spans to its sinks. It is safe for
// concurrent use: every event is numbered and delivered under one lock, so
// each sink sees the same order.
type Bus struct {
	// suspending keeps one caller's Suspend or Resume calls to the sinks
	// from interleaving with another's.
	suspending sync.Mutex

	mu       sync.Mutex
	sinks    []Sink
	lines    bool
	now      func() time.Time
	panicLog io.Writer
	trace    TraceID
	base     uint64
	started  uint64
	seq      uint64
	// last is the newest of the open spans, which are linked in the order
	// they started, so that the innermost come last.
	last      *Span
	suspended int
	closed    bool
}

// NewBus returns a Bus that sends its events to o.Sinks.
func NewBus(o Options) *Bus {
	b := &Bus{
		sinks:    slices.Clone(o.Sinks),
		now:      o.Now,
		panicLog: o.PanicLog,
		trace:    o.Trace,
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.trace == (TraceID{}) {
		// crypto/rand never fails; it ends the process when it cannot.
		_, _ = rand.Read(b.trace[:])
	}
	var base [8]byte
	_, _ = rand.Read(base[:])
	b.base = binary.BigEndian.Uint64(base[:])
	b.mu.Lock()
	b.lines = b.wantLines()
	b.mu.Unlock()
	return b
}

// Trace returns the trace the Bus's spans belong to.
func (b *Bus) Trace() TraceID { return b.trace }

// Close ends every span still open as canceled, "not finished", the
// innermost first, and resumes a display still suspended. Nothing is sent
// after Close; the sinks are not closed, which is for whoever made them to
// do once Close has returned. Closing a closed Bus does nothing.
func (b *Bus) Close() {
	b.suspending.Lock()
	defer b.suspending.Unlock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	for b.last != nil {
		b.last.end(StatusCanceled, ClassCanceled, notFinished, nil)
	}
	resumes := b.suspended
	for range resumes {
		b.emit(Event{Type: TypeResume})
	}
	b.suspended = 0
	b.closed = true
	sus := b.suspenders()
	b.mu.Unlock()
	for range resumes {
		b.callAll(sus, Suspender.Resume)
	}
}

// notFinished is the error of a span ended because its parent or the Bus
// ended first.
const notFinished = "not finished"

// Span is one unit of work. A nil *Span is one nobody watches, and every
// method of it does nothing. The methods are safe for concurrent use.
type Span struct {
	bus    *Bus
	parent *Span
	id     SpanID
	kind   Kind
	name   string
	flags  Flags

	// The rest is guarded by bus.mu.
	state  State
	fields Fields
	// open counts the spans started under this one that have not ended.
	open int
	// prev and next link the Bus's open spans.
	prev, next *Span
	lines      *lineState
}

type busKey struct{}

type spanKey struct{}

// WithBus returns a context that carries b, so that the spans started under
// it are sent to b's sinks. A span of another Bus the context carries is no
// parent to them.
func WithBus(ctx context.Context, b *Bus) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, busKey{}, b)
}

// BusFrom returns the Bus ctx carries, or nil.
func BusFrom(ctx context.Context) *Bus {
	b, _ := ctx.Value(busKey{}).(*Bus)
	return b
}

// SpanFrom returns the innermost span ctx carries, or nil.
func SpanFrom(ctx context.Context) *Span {
	_, s := from(ctx)
	return s
}

// from returns the Bus ctx carries and its innermost span in it.
func from(ctx context.Context) (*Bus, *Span) {
	b := BusFrom(ctx)
	if b == nil {
		return nil, nil
	}
	s, _ := ctx.Value(spanKey{}).(*Span)
	if s != nil && s.bus != b {
		s = nil
	}
	return b, s
}

// Start begins a span under the innermost one ctx carries, and returns a
// context that carries the new span along with it. The span is running
// unless Queued is given. Without a Bus in ctx, once it is closed, or under
// a span that has ended, Start returns ctx itself and a nil span: what a
// worker starts after its target was ended for it, by the end of its step
// or of the command, belongs to nothing still open, and would outlive the
// span its ending ended.
func Start(ctx context.Context, k Kind, name string, opts ...Option) (context.Context, *Span) {
	b, parent := from(ctx)
	if b == nil {
		return ctx, nil
	}
	var o options
	o.apply(opts)
	o.Fields = sanitizeFields(o.Fields, Fields{})
	s := &Span{
		bus:    b,
		parent: parent,
		kind:   k,
		name:   Sanitize(name, MaxField),
		flags:  o.flags,
		state:  StateRunning,
		fields: o.Fields,
	}
	if o.queued {
		s.state = StateQueued
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || parent != nil && parent.state == StateEnded {
		return ctx, nil
	}
	if parent != nil {
		s.flags |= parent.flags & inherited
		parent.open++
	}
	b.started++
	s.id = SpanID(b.base + b.started)
	if s.id == 0 {
		b.started++
		s.id = SpanID(b.base + b.started)
	}
	s.prev = b.last
	if b.last != nil {
		b.last.next = s
	}
	b.last = s
	b.emit(s.event(TypeStart))
	return context.WithValue(ctx, spanKey{}, s), s
}

// Run marks a queued span as running, when its work takes its place in a
// pool. It does nothing to a span that is running or ended.
func (s *Span) Run() {
	if s == nil {
		return
	}
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || s.state != StateQueued {
		return
	}
	s.state = StateRunning
	b.emit(s.event(TypeRun))
}

// Update changes the Total and Message of a span that has not ended. A
// Total smaller than the one before is ignored, and so are other options.
func (s *Span) Update(opts ...Option) {
	if s == nil {
		return
	}
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || s.state == StateEnded {
		return
	}
	o := options{Fields: s.fields}
	o.apply(opts)
	s.fields.Total = max(s.fields.Total, o.Total)
	if o.Message != s.fields.Message {
		s.fields.Message = Sanitize(o.Message, MaxField)
	}
	b.emit(s.event(TypeUpdate))
}

// End ends a span with the outcome of its work: ok for a nil error,
// otherwise failed or canceled as Classify tells from err, whose text
// becomes the span's one-line Err. The options set the fields that are
// known only at the end, such as Exit. Only the first End or Skip of a span
// counts. Spans started under it that are still open end first, as
// canceled, "not finished".
func (s *Span) End(err error, opts ...Option) {
	if s == nil {
		return
	}
	status, class, text := StatusOK, ClassNone, ""
	if err != nil {
		class = Classify(err)
		status = StatusFailed
		if class == ClassCanceled {
			status = StatusCanceled
		}
		text = Sanitize(err.Error(), MaxErr)
	}
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	s.end(status, class, text, opts)
}

// Skip ends a queued or running span as skipped, with reason as its Err:
// work left out on purpose, such as a batch after one that failed or a
// call a dry run only recorded. It is the span's End, and like End only
// counts the first time.
func (s *Span) Skip(reason string) {
	if s == nil {
		return
	}
	text := Sanitize(reason, MaxErr)
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	s.end(StatusSkipped, ClassNone, text, nil)
}

// end ends s, and the spans still open under it first. b.mu is held.
func (s *Span) end(status Status, class Class, text string, opts []Option) {
	b := s.bus
	if s.state == StateEnded {
		return
	}
	if s.open > 0 {
		// The innermost first: a span that started later comes later in
		// the list, and its own children have ended by the time it is
		// reached.
		for o := b.last; o != nil && s.open > 0; {
			prev := o.prev
			if o.under(s) {
				o.end(StatusCanceled, ClassCanceled, notFinished, nil)
			}
			o = prev
		}
	}
	dropped := s.flushLines()
	if len(opts) > 0 {
		o := options{Fields: s.fields}
		o.apply(opts)
		o.Total = max(o.Total, s.fields.Total)
		s.fields = sanitizeFields(o.Fields, s.fields)
	}
	s.state = StateEnded
	if s.prev != nil {
		s.prev.next = s.next
	}
	if s.next != nil {
		s.next.prev = s.prev
	} else {
		b.last = s.prev
	}
	s.prev, s.next = nil, nil
	if s.parent != nil {
		s.parent.open--
	}
	e := s.event(TypeEnd)
	e.Status, e.Class, e.Err, e.Dropped = status, class, text, dropped
	b.emit(e)
}

// under reports whether s was started below ancestor.
func (s *Span) under(ancestor *Span) bool {
	for p := s.parent; p != nil; p = p.parent {
		if p == ancestor {
			return true
		}
	}
	return false
}

// event returns an event about s as it is now. b.mu is held.
func (s *Span) event(t Type) Event {
	e := Event{
		Type:   t,
		Span:   s.id,
		Kind:   s.kind,
		Name:   s.name,
		Flags:  s.flags,
		State:  s.state,
		Fields: s.fields,
	}
	if s.parent != nil {
		e.Parent = s.parent.id
	}
	return e
}

// Suspend takes every display off the terminal, for a question to be asked
// there, and returns once they are all off. Calling the function it returns
// puts them back; calling that again does nothing. Suspensions may nest,
// and without a Bus in ctx nothing happens.
func Suspend(ctx context.Context) (resume func()) {
	b, s := from(ctx)
	if b == nil {
		return func() {}
	}
	var id SpanID
	if s != nil {
		id = s.id
	}
	if !b.suspend(id) {
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(func() { b.resume(id) }) }
}

func (b *Bus) suspend(id SpanID) bool {
	b.suspending.Lock()
	defer b.suspending.Unlock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	b.suspended++
	b.emit(Event{Type: TypeSuspend, Span: id})
	sus := b.suspenders()
	b.mu.Unlock()
	b.callAll(sus, Suspender.Suspend)
	return true
}

func (b *Bus) resume(id SpanID) {
	b.suspending.Lock()
	defer b.suspending.Unlock()
	b.mu.Lock()
	// Close has resumed whatever it found suspended.
	if b.closed || b.suspended == 0 {
		b.mu.Unlock()
		return
	}
	b.suspended--
	b.emit(Event{Type: TypeResume, Span: id})
	sus := b.suspenders()
	b.mu.Unlock()
	b.callAll(sus, Suspender.Resume)
}

// suspenders returns the sinks that draw on the terminal. b.mu is held.
func (b *Bus) suspenders() []Suspender {
	var out []Suspender
	for _, s := range b.sinks {
		if x, ok := s.(Suspender); ok {
			out = append(out, x)
		}
	}
	return out
}

// callAll calls call for each of sus, outside b.mu, and removes one that
// panics.
func (b *Bus) callAll(sus []Suspender, call func(Suspender)) {
	for _, x := range sus {
		if !b.safely(func() { call(x) }) {
			b.mu.Lock()
			b.remove(x.(Sink))
			b.mu.Unlock()
		}
	}
}

// emit numbers e and hands it to every sink. b.mu is held.
func (b *Bus) emit(e Event) {
	b.seq++
	e.Seq = b.seq
	e.Time = b.now()
	for i := 0; i < len(b.sinks); i++ {
		sink := b.sinks[i]
		if !b.safely(func() { sink.Handle(e) }) {
			b.remove(sink)
			i--
		}
	}
}

// safely calls f and reports whether it returned rather than panicked; the
// stack of a panic goes to the panic log. A sink is part of how the work is
// shown, not of the work, so a sink that fails must not take the work with
// it.
func (b *Bus) safely(f func()) (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			log := b.panicLog
			if log == nil {
				log = os.Stderr
			}
			// The log is a courtesy; a write that fails changes nothing.
			_, _ = fmt.Fprintf(log, "clusterctl: a progress display panicked and was stopped: %q\n%s", fmt.Sprint(v), debug.Stack())
			ok = false
		}
	}()
	f()
	return true
}

// remove takes sink off the Bus. b.mu is held.
func (b *Bus) remove(sink Sink) {
	b.sinks = slices.DeleteFunc(b.sinks, func(s Sink) bool { return s == sink })
	b.lines = b.wantLines()
}

// wantLines reports whether a sink asks for lines. b.mu is held.
func (b *Bus) wantLines() bool {
	for _, s := range b.sinks {
		want := false
		if ls, ok := s.(LineSink); ok && b.safely(func() { want = ls.WantsLines() }) && want {
			return true
		}
	}
	return false
}

// sanitizeFields makes the text fields of f that differ from those of prev
// safe to show.
func sanitizeFields(f, prev Fields) Fields {
	clean := func(s *string, was string, max int) {
		if *s != was {
			*s = Sanitize(*s, max)
		}
	}
	// Node is a node set a display folds, so it is escaped but never cut.
	clean(&f.Node, prev.Node, 0)
	clean(&f.Host, prev.Host, MaxField)
	clean(&f.Role, prev.Role, MaxField)
	clean(&f.Batch, prev.Batch, MaxField)
	clean(&f.Message, prev.Message, MaxField)
	clean(&f.Method, prev.Method, MaxField)
	clean(&f.Path, prev.Path, MaxField)
	clean(&f.Cache, prev.Cache, MaxField)
	clean(&f.Source, prev.Source, MaxField)
	return f
}
