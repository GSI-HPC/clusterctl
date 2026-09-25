// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// lineBurst lines of a call are sent at once, and then one each
	// lineEvery, so that a command that prints fast does not flood the
	// sinks: at a fan-out of 16 that is at most 160 lines a second.
	lineBurst = 20
	lineEvery = 100 * time.Millisecond
	// cutLine is how much of a line without an end a display is sent at a
	// time.
	cutLine = 4 << 10
	// maxParsedLine is the longest line handed to a parser. A longer one
	// is no answer any parser here reads, and holding it would take as
	// much memory as the output.
	maxParsedLine = 64 << 10
)

// lineState is the rate limit of a span's lines, guarded by bus.mu.
type lineState struct {
	tokens int
	last   time.Time
	// pending is the newest line that found no token, sent with the next
	// token or at the End, so that the last line a display shows is never
	// an old one; late sends it once the next token is due when no line
	// comes before.
	pending    string
	hasPending bool
	pendingOn  Stream
	late       *time.Timer
	// since counts the lines left out since the last one sent, and
	// dropped all of them.
	since, dropped int
	tees           []*tee
}

// Tee returns a writer that writes to w, and cuts what it writes into
// lines on the way.
//
// Each complete line goes to parse, when it is not nil, with its "\n" and a
// "\r" before it taken off, from the goroutine that writes; the tees of a
// command's two streams may call it at the same time. A parser sees only
// lines that ended: a line cut off by the end of the output, or longer than
// 64 KiB, is none it is given.
//
// When the innermost span in ctx has ShowLines and a sink wants lines, they
// also go to the sinks as TypeLine events, sanitised and rate limited: a
// burst of 20, then one each 100ms. A line that finds no place waits for
// the next one, in place of the line that waited before it, which is
// counted as dropped; so the newest line is never lost, and is sent when
// its place comes, whether another line comes or not. A line that does
// not end is sent in pieces of 4 KiB, and what is left of it when the span
// ends is sent before the End.
//
// Tee returns w itself when nothing needs the lines. The lines never fail a
// write: what Write returns is what w returned.
func Tee(ctx context.Context, w io.Writer, st Stream, parse func(Stream, string)) io.Writer {
	s := SpanFrom(ctx)
	if s != nil && s.flags&ShowLines == 0 {
		s = nil
	}
	if s == nil && parse == nil {
		return w
	}
	t := &tee{w: w, stream: st, parse: parse, span: s}
	if s != nil && !s.watch(t) {
		t.span = nil
		if parse == nil {
			return w
		}
	}
	return t
}

// watch registers t to be sent s's lines, if a sink wants them and s has
// not ended.
func (s *Span) watch(t *tee) bool {
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || !b.lines || s.state == StateEnded {
		return false
	}
	ls := s.lineState()
	ls.tees = append(ls.tees, t)
	return true
}

func (s *Span) lineState() *lineState {
	if s.lines == nil {
		s.lines = &lineState{tokens: lineBurst, last: s.bus.now()}
	}
	return s.lines
}

type tee struct {
	w      io.Writer
	stream Stream
	parse  func(Stream, string)

	mu sync.Mutex
	// span is nil when no display wants the lines, or once it has ended.
	span *Span
	// parsed is the line so far for the parser, and long is set when it
	// is too long to be one.
	parsed []byte
	long   bool
	// shown is the part of the line so far not yet sent to the display.
	shown []byte
}

func (t *tee) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 && n <= len(p) {
		t.frame(p[:n])
	}
	return n, err
}

// frame cuts p into lines, and hands them on once t.mu is let go of: a
// span's End takes t.mu while it holds the Bus lock, so the lock is never
// taken the other way round.
func (t *tee) frame(p []byte) {
	var parsed, shown []string
	t.mu.Lock()
	span := t.span
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if t.parse != nil && !t.long {
			if len(t.parsed)+len(part) > maxParsedLine {
				t.long, t.parsed = true, t.parsed[:0]
			} else {
				t.parsed = append(t.parsed, part...)
			}
		}
		if span != nil {
			t.shown = append(t.shown, part...)
			for len(t.shown) >= cutLine {
				k := runeCut(t.shown, cutLine)
				shown = append(shown, string(t.shown[:k]))
				t.shown = append(t.shown[:0], t.shown[k:]...)
			}
		}
		if i < 0 {
			break
		}
		if t.parse != nil {
			if !t.long {
				parsed = append(parsed, string(bytes.TrimSuffix(t.parsed, []byte("\r"))))
			}
			t.parsed, t.long = t.parsed[:0], false
		}
		if span != nil && len(t.shown) > 0 {
			shown = append(shown, string(t.shown))
			t.shown = t.shown[:0]
		}
		p = p[i+1:]
	}
	t.mu.Unlock()

	for _, line := range parsed {
		t.parse(t.stream, line)
	}
	if span != nil {
		span.show(t.stream, shown)
	}
}

// runeCut returns where to cut p to at most n bytes without splitting a
// rune.
func runeCut[T string | []byte](p T, n int) int {
	if n >= len(p) {
		return len(p)
	}
	for i := n; i > 0 && i > n-utf8.UTFMax; i-- {
		if utf8.RuneStart(p[i]) {
			return i
		}
	}
	// Continuation bytes that follow no start are no rune to split.
	return n
}

// show offers lines to the sinks, then the line held back if a token has
// come free since.
func (s *Span) show(st Stream, lines []string) {
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || s.state == StateEnded {
		return
	}
	ls := s.lineState()
	ls.refill(b.now())
	for _, line := range lines {
		s.offer(st, line)
	}
	if ls.hasPending && ls.tokens > 0 {
		ls.tokens--
		s.sendPending()
	}
}

// offer sends line if a token is free, and holds it back otherwise, in
// place of any line held back before it. b.mu is held.
func (s *Span) offer(st Stream, line string) {
	ls := s.lines
	if ls.hasPending {
		// The newest line wins; the one before it is not sent.
		ls.since++
		ls.dropped++
		ls.hasPending = false
	}
	if ls.tokens > 0 {
		ls.tokens--
		s.sendLine(st, line)
		return
	}
	ls.pending, ls.pendingOn, ls.hasPending = line, st, true
	s.sendLate()
}

// sendLate has the line held back sent once the next token is due, unless
// another line or the End sends it first. b.mu is held.
func (s *Span) sendLate() {
	ls := s.lines
	if ls.late != nil {
		return
	}
	wait := lineEvery - s.bus.now().Sub(ls.last)
	if wait <= 0 {
		wait = lineEvery
	}
	ls.late = time.AfterFunc(wait, func() {
		b := s.bus
		b.mu.Lock()
		defer b.mu.Unlock()
		ls.late = nil
		if b.closed || s.state == StateEnded || !ls.hasPending {
			return
		}
		ls.refill(b.now())
		if ls.tokens > 0 {
			ls.tokens--
			s.sendPending()
			return
		}
		s.sendLate()
	})
}

func (s *Span) sendPending() {
	ls := s.lines
	ls.hasPending = false
	s.sendLine(ls.pendingOn, ls.pending)
	ls.pending = ""
}

// sendLine sends one line. b.mu is held.
func (s *Span) sendLine(st Stream, line string) {
	e := s.event(TypeLine)
	e.Stream, e.Text, e.Dropped = st, Sanitize(line, MaxText), s.lines.since
	s.lines.since = 0
	s.bus.emit(e)
}

// refill adds the tokens that have come due since the last one was
// spent.
func (ls *lineState) refill(now time.Time) {
	if ls.tokens >= lineBurst {
		ls.last = now
		return
	}
	n := int(now.Sub(ls.last) / lineEvery)
	if n <= 0 {
		return
	}
	ls.tokens = min(lineBurst, ls.tokens+n)
	ls.last = ls.last.Add(time.Duration(n) * lineEvery)
}

// flushLines sends what the tees of s hold of an unfinished line and the
// line held back, before s ends, and returns how many lines were dropped
// in all. b.mu is held.
func (s *Span) flushLines() int {
	ls := s.lines
	if ls == nil {
		return 0
	}
	ls.refill(s.bus.now())
	for _, t := range ls.tees {
		t.mu.Lock()
		rest := string(t.shown)
		t.span, t.shown = nil, nil
		t.mu.Unlock()
		if rest != "" {
			s.offer(t.stream, rest)
		}
	}
	ls.tees = nil
	if ls.hasPending {
		s.sendPending()
	}
	if ls.late != nil {
		ls.late.Stop()
		ls.late = nil
	}
	return ls.dropped
}
