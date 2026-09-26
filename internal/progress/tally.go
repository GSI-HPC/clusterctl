// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress

// Tally keeps count of the targets of a Bus's work, from its events, the
// way a display shows how far the work has got.
//
// A step with Fold and a batch count the targets below them. A target that
// ends counts once in every such span above it, however it ended; a batch
// or a Fold step left out, skipped or canceled before any target of its own
// started, counts in the same spans as its Total. So a count reaches its
// Total once all the work it expects has ended, after an interrupt too,
// and never passes it: progresstest.Check holds every source to that.
//
// The spans that count with none above them are the roots of the tally,
// what a counter shows: "power on" as a whole, not each of its batches.
//
// A Tally is given every event of a Bus in order, as a sink is, and forgets
// a span once it has ended. It is not safe for concurrent use; a sink
// feeds it from Handle, under the Bus lock.
type Tally struct {
	spans map[SpanID]*tallied
	// roots are the open roots, in the order they started.
	roots []*tallied
}

// Count is where the targets below a step or a batch that counts them
// stand.
type Count struct {
	Span  SpanID
	Name  string
	Flags Flags
	// Total is the span's Total, the targets it expects.
	Total int
	// Done counts the targets that ended below, however they ended, and
	// the Total of every batch or Fold step below that was left out.
	// Failed, Canceled and Skipped count those that ended so.
	Done, Failed, Canceled, Skipped int
	// Running counts the targets below that are running. Queued counts
	// those that wait for their turn, and the Total of every batch below
	// that waits for its own.
	Running, Queued int
	// Targets counts the targets that started below, queued or running.
	Targets int
	// Batch is the place, "i/n", of the batch below that ran last, or of
	// the span itself when it is a batch.
	Batch string
	// Waits counts the waits open below, such as a pause between batches.
	Waits int
}

type tallied struct {
	parent *tallied
	kind   Kind
	state  State
	// counts says the span counts the targets below it, in count.
	counts bool
	count  Count
	// queued is what the span adds to the Queued of the spans above it
	// while it waits: one for a target, its Total for a batch.
	queued int
}

// counts reports whether a span of kind k with flags f counts the targets
// below it.
func counts(k Kind, f Flags) bool {
	return k == KindBatch || k == KindStep && f&Fold != 0
}

// Add counts e in. When e ends a span that counts, Add returns where its
// targets stood as it ended.
func (t *Tally) Add(e Event) (ended Count, ok bool) {
	switch e.Type {
	case TypeStart:
		t.start(e)
	case TypeRun:
		t.run(e)
	case TypeUpdate:
		if s := t.spans[e.Span]; s != nil {
			t.grow(s, e.Total)
		}
	case TypeEnd:
		return t.end(e)
	}
	return Count{}, false
}

// Roots returns where the open roots stand, in the order they started.
func (t *Tally) Roots() []Count {
	out := make([]Count, 0, len(t.roots))
	for _, r := range t.roots {
		out = append(out, r.count)
	}
	return out
}

// Count returns where the targets below span stand, when it is open and
// counts them.
func (t *Tally) Count(span SpanID) (Count, bool) {
	s := t.spans[span]
	if s == nil || !s.counts {
		return Count{}, false
	}
	return s.count, true
}

func (t *Tally) start(e Event) {
	if t.spans == nil {
		t.spans = map[SpanID]*tallied{}
	}
	if _, again := t.spans[e.Span]; again {
		return
	}
	s := &tallied{kind: e.Kind, state: e.State}
	if e.Parent != 0 {
		s.parent = t.spans[e.Parent]
	}
	t.spans[e.Span] = s
	if counts(e.Kind, e.Flags) {
		s.counts = true
		s.count = Count{Span: e.Span, Name: e.Name, Flags: e.Flags, Total: e.Total}
		if e.Kind == KindBatch {
			s.count.Batch = e.Batch
		}
		if !s.below() {
			t.roots = append(t.roots, s)
		}
	}
	switch e.Kind {
	case KindTarget:
		s.above(func(c *Count) { c.Targets++ })
		if e.State == StateQueued {
			s.queued = 1
		} else {
			s.above(func(c *Count) { c.Running++ })
		}
	case KindBatch:
		if e.State == StateQueued {
			s.queued = e.Total
		}
	case KindWait:
		s.above(func(c *Count) { c.Waits++ })
	}
	s.above(func(c *Count) { c.Queued += s.queued })
}

func (t *Tally) run(e Event) {
	s := t.spans[e.Span]
	if s == nil || s.state != StateQueued {
		return
	}
	s.state = StateRunning
	waited := s.queued
	s.queued = 0
	s.above(func(c *Count) {
		c.Queued -= waited
		switch s.kind {
		case KindTarget:
			c.Running++
		case KindBatch:
			c.Batch = e.Batch
		}
	})
}

// grow raises the Total of s, and what a batch that waits adds to the
// Queued of the spans above it.
func (t *Tally) grow(s *tallied, total int) {
	if s.counts {
		s.count.Total = max(s.count.Total, total)
	}
	if s.kind == KindBatch && s.state == StateQueued && total > s.queued {
		more := total - s.queued
		s.queued = total
		s.above(func(c *Count) { c.Queued += more })
	}
}

func (t *Tally) end(e Event) (Count, bool) {
	s := t.spans[e.Span]
	if s == nil {
		return Count{}, false
	}
	delete(t.spans, e.Span)
	t.grow(s, e.Total)
	waited, was := s.queued, s.state
	s.queued, s.state = 0, StateEnded
	s.above(func(c *Count) { c.Queued -= waited })
	switch s.kind {
	case KindTarget:
		s.above(func(c *Count) {
			if was == StateRunning {
				c.Running--
			}
			c.Done++
			tallyStatus(c, e.Status, 1)
		})
	case KindWait:
		s.above(func(c *Count) { c.Waits-- })
	}
	if !s.counts {
		return Count{}, false
	}
	if s.count.Targets == 0 && (e.Status == StatusSkipped || e.Status == StatusCanceled) {
		left := s.count.Total
		s.above(func(c *Count) {
			c.Done += left
			tallyStatus(c, e.Status, left)
		})
	}
	for i, r := range t.roots {
		if r == s {
			t.roots = append(t.roots[:i], t.roots[i+1:]...)
			break
		}
	}
	return s.count, true
}

// tallyStatus counts n targets that ended with status into c.
func tallyStatus(c *Count, status Status, n int) {
	switch status {
	case StatusFailed:
		c.Failed += n
	case StatusCanceled:
		c.Canceled += n
	case StatusSkipped:
		c.Skipped += n
	}
}

// above calls f with the count of every span above s that counts.
func (s *tallied) above(f func(*Count)) {
	for p := s.parent; p != nil; p = p.parent {
		if p.counts {
			f(&p.count)
		}
	}
}

// below reports whether s was started below a span that counts.
func (s *tallied) below() bool {
	for p := s.parent; p != nil; p = p.parent {
		if p.counts {
			return true
		}
	}
	return false
}
