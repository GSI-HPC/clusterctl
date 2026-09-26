// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

const (
	// plainBeat is how often a Plain says how far each counted step under
	// way has got.
	plainBeat = 10 * time.Second
	// plainEvery is how often a Plain looks whether one is due.
	plainEvery = time.Second
)

// Plain is a display of plain lines, one for each thing worth a line, with
// the time since the command started in front and no escape codes, for a
// log as much as for a terminal:
//
//	[0:00] bmc power › power off: start, 480 hosts, 8 at a time
//	[0:03] bmc power › power off › exe0007 failed (transport): exe0007.mgmt: dial tcp: i/o timeout
//	[0:10] bmc power › power off: 312/480 done, 1 failed, 8 running, 160 queued
//	[0:18] bmc power › power off: failed in 18s: 478 ok, 2 failed
//
// A line names the span it tells of by the path to it from the command. It
// is written as a step or a batch starts and ends, as a pause of a known
// length starts, and as a wait fails or is interrupted; for each
// target that fails, once; and, every ten seconds, for each counted step
// under way, a root of a progress.Tally, with how far it has got. Hidden
// spans and calls get no line, and neither do the lines of output the work
// prints: exec prints them at the end, and the machine formats carry them
// whole.
//
// The lines go out through the Terminal: ahead of whatever the command
// writes after the events they tell of, never into a line the command has
// not ended, and not while a question is asked. A Plain is a progress.Sink
// and a progress.Suspender; its methods are safe for concurrent use.
type Plain struct {
	term  *Terminal
	now   func() time.Time
	start time.Time

	mu    sync.Mutex
	tally progress.Tally
	// spans are the open spans.
	spans map[progress.SpanID]*plainSpan
	// beats are the counted steps under way that no counted span is
	// above, in the order they started, with when each is due to say
	// how far it has got.
	beats []beat
	// lines are the lines not yet written, each ended by a newline.
	lines strings.Builder
	// suspended counts the Suspends not yet resumed, during which no
	// heartbeat falls due.
	suspended int

	wake          chan struct{}
	stop, stopped chan struct{}
	closing       sync.Once
}

type plainSpan struct {
	kind progress.Kind
	// path names the span from the command down.
	path   string
	hidden bool
	// counts says the span counts the targets below it, and below says a
	// span above it does.
	counts, below bool
	// ran is when the span started running; zero while it waits.
	ran time.Time
}

type beat struct {
	span progress.SpanID
	due  time.Time
}

// PlainOptions configure a Plain.
type PlainOptions struct {
	// Now is the clock the heartbeats are read from, and the time in front
	// of a line counted from; nil is time.Now. The Bus's clock should be
	// the same.
	Now func() time.Time
}

// NewPlain returns a display of plain lines on term, which counts the time
// in front of its lines from now.
func NewPlain(term *Terminal, o PlainOptions) *Plain {
	p := &Plain{term: term, now: o.Now, spans: map[progress.SpanID]*plainSpan{}}
	if p.now == nil {
		p.now = time.Now
	}
	p.start = p.now()
	term.mu.Lock()
	term.held = p.take
	term.mu.Unlock()
	return p
}

// Start writes the lines as they come, and the heartbeats as they fall due,
// until Close.
func (p *Plain) Start() {
	p.mu.Lock()
	p.wake = make(chan struct{}, 1)
	p.mu.Unlock()
	p.stop, p.stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.stopped)
		defer p.term.recovered()
		tick := time.NewTicker(plainEvery)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-p.wake:
				p.term.flush()
			case <-tick.C:
				p.Draw()
			}
		}
	}()
}

// Draw writes the lines not yet written and the heartbeats due now, where
// the terminal lets it.
func (p *Plain) Draw() {
	now := p.now()
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.suspended == 0 {
			p.heartbeats(now)
		}
	}()
	p.term.flush()
}

// Close stops the writing Start began, and writes what is left. Closing a
// closed Plain does nothing.
func (p *Plain) Close() {
	p.closing.Do(func() {
		if p.stop != nil {
			close(p.stop)
			<-p.stopped
		}
		p.term.close()
	})
}

// Suspend writes the lines not yet written, and then no more until Resume.
func (p *Plain) Suspend() {
	p.mu.Lock()
	p.suspended++
	p.mu.Unlock()
	p.term.suspend()
}

// Resume lets the lines out again.
func (p *Plain) Resume() {
	p.mu.Lock()
	if p.suspended > 0 {
		p.suspended--
	}
	p.mu.Unlock()
	p.term.resume()
}

// take returns the lines not yet written, and forgets them. The terminal's
// lock is held.
func (p *Plain) take() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	text := p.lines.String()
	p.lines.Reset()
	return text
}

// Handle turns e into its line, if it is worth one.
func (p *Plain) Handle(e progress.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	count, counted := p.tally.Add(e)
	switch e.Type {
	case progress.TypeStart:
		p.begin(e)
	case progress.TypeRun:
		if s := p.spans[e.Span]; s != nil {
			p.run(e, s)
		}
	case progress.TypeEnd:
		if s := p.spans[e.Span]; s != nil {
			delete(p.spans, e.Span)
			p.end(e, s, count, counted)
		}
	}
}

// begin keeps a span that starts, and says so when it starts running.
func (p *Plain) begin(e progress.Event) {
	s := &plainSpan{
		kind:   e.Kind,
		hidden: e.Flags&progress.Hidden != 0,
		counts: e.Kind == progress.KindBatch || e.Kind == progress.KindStep && e.Flags&progress.Fold != 0,
	}
	parent := p.spans[e.Parent]
	if parent != nil {
		s.path = parent.path
		s.below = parent.counts || parent.below
	}
	// A call names no part of the path: nothing below one gets a line of
	// its own but through its target.
	if e.Kind != progress.KindCall {
		name := e.Name
		if e.Kind == progress.KindBatch {
			name = "batch " + name
		}
		if s.path != "" {
			name = s.path + " › " + name
		}
		s.path = name
	}
	p.spans[e.Span] = s
	if e.State == progress.StateRunning {
		p.run(e, s)
	}
}

// run says that a span has started running.
func (p *Plain) run(e progress.Event, s *plainSpan) {
	s.ran = e.Time
	if s.hidden {
		return
	}
	switch e.Kind {
	case progress.KindStep, progress.KindBatch:
		p.line(e.Time, s.path+": start"+sizes(e.Fields))
		if s.counts && !s.below {
			p.beats = append(p.beats, beat{span: e.Span, due: e.Time.Add(plainBeat)})
		}
	case progress.KindWait:
		if e.Timeout > 0 {
			p.line(e.Time, fmt.Sprintf("%s: waiting %s", s.path, e.Timeout))
		}
	}
}

// end says how a span ended, if that is worth a line.
func (p *Plain) end(e progress.Event, s *plainSpan, count progress.Count, counted bool) {
	for i, b := range p.beats {
		if b.span == e.Span {
			p.beats = append(p.beats[:i], p.beats[i+1:]...)
			break
		}
	}
	if s.hidden {
		return
	}
	switch e.Kind {
	case progress.KindTarget:
		if e.Status == progress.StatusFailed {
			text := fmt.Sprintf("%s failed (%s)", s.path, e.Class)
			if e.Err != "" {
				text += ": " + e.Err
			}
			p.line(e.Time, text)
		}
	case progress.KindStep, progress.KindBatch:
		if s.ran.IsZero() {
			// Left out before it ran, as the batches after one that
			// failed are.
			p.line(e.Time, s.path+": "+outcome(e))
			return
		}
		text := fmt.Sprintf("%s: %s in %s", s.path, word(e.Status), took(e.Time.Sub(s.ran)))
		if counted {
			text += ": " + ended(count)
		}
		p.line(e.Time, text)
	case progress.KindWait:
		if e.Status == progress.StatusFailed || e.Status == progress.StatusCanceled {
			p.line(e.Time, s.path+": "+outcome(e))
		}
	}
}

// heartbeats says, for each counted step under way that is due, how far it
// has got. p.mu is held.
func (p *Plain) heartbeats(now time.Time) {
	for i := range p.beats {
		b := &p.beats[i]
		if now.Before(b.due) {
			continue
		}
		for !now.Before(b.due) {
			b.due = b.due.Add(plainBeat)
		}
		s := p.spans[b.span]
		count, ok := p.tally.Count(b.span)
		if s == nil || !ok {
			continue
		}
		p.line(now, s.path+": "+standing(count))
	}
}

// line adds a line at t to those not yet written, and has them written. p.mu
// is held.
func (p *Plain) line(t time.Time, text string) {
	fmt.Fprintf(&p.lines, "[%s] %s\n", elapsed(max(0, t.Sub(p.start))), text)
	if p.wake != nil {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// sizes says how many targets a step or a batch expects, and how many of
// them it works on at once when that is fewer.
func sizes(f progress.Fields) string {
	if f.Total <= 0 {
		return ""
	}
	text := ", " + hosts(f.Total)
	if f.Limit > 0 && f.Limit < f.Total {
		text += fmt.Sprintf(", %d at a time", f.Limit)
	}
	return text
}

func hosts(n int) string {
	if n == 1 {
		return "1 host"
	}
	return fmt.Sprintf("%d hosts", n)
}

// standing says how far a counted step has got while it is under way.
func standing(c progress.Count) string {
	var parts []string
	if c.Batch != "" {
		parts = append(parts, "batch "+c.Batch)
	}
	parts = append(parts, fmt.Sprintf("%d/%d done", c.Done, c.Total))
	for _, n := range []struct {
		n    int
		what string
	}{
		{c.Failed, "failed"},
		{c.Canceled, "canceled"},
		{c.Skipped, "skipped"},
		{c.Running, "running"},
		{c.Queued, "queued"},
	} {
		if n.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n.n, n.what))
		}
	}
	if c.Waits > 0 {
		parts = append(parts, "waiting")
	}
	return strings.Join(parts, ", ")
}

// ended says how the targets of a counted step or batch ended.
func ended(c progress.Count) string {
	return tallied(c.Done-c.Failed-c.Canceled-c.Skipped, c.Failed, c.Canceled, c.Skipped)
}

// tallied says how many ended how: "478 ok, 2 failed", leaving out none but
// ok.
func tallied(ok, failed, canceled, skipped int) string {
	parts := []string{fmt.Sprintf("%d ok", ok)}
	for _, n := range []struct {
		n    int
		what string
	}{{failed, "failed"}, {canceled, "canceled"}, {skipped, "skipped"}} {
		if n.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n.n, n.what))
		}
	}
	return strings.Join(parts, ", ")
}

// outcome says in a word how a span ended that has no count to say, with
// the reason when it was left out: that is no failure, which report prints.
func outcome(e progress.Event) string {
	switch e.Status {
	case progress.StatusFailed:
		return fmt.Sprintf("failed (%s)", e.Class)
	case progress.StatusSkipped:
		if e.Err != "" {
			return "skipped: " + e.Err
		}
	}
	return word(e.Status)
}

// word says in a word how a span ended.
func word(s progress.Status) string {
	switch s {
	case progress.StatusOK:
		return "done"
	case progress.StatusFailed:
		return "failed"
	case progress.StatusCanceled:
		return "canceled"
	case progress.StatusSkipped:
		return "skipped"
	}
	return s.String()
}
