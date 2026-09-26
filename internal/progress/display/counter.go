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
	// counterDelay is how long a command runs before its counter is drawn:
	// a command that is done by then never shows one.
	counterDelay = time.Second
	// counterEvery is how often the counter is drawn at most.
	counterEvery = 100 * time.Millisecond
)

// Counter is a display of one line, which says for each step whose targets
// are counted, the roots of a progress.Tally, how far it has got, and how
// long the command has run:
//
//	power on · batch 3/60 · 17/480 · 1 failed · 8 running · 0:41
//
// Steps under way side by side each get a segment of their own, split by
// " | ". While no counted step is under way the line names the innermost
// step that is, or the command. Hidden steps are not shown.
//
// A Counter is a progress.Sink and a progress.Suspender. Its methods are
// safe for concurrent use.
type Counter struct {
	term  *Terminal
	now   func() time.Time
	start time.Time

	mu    sync.Mutex
	tally progress.Tally
	// named are the open steps and the command, the newest last, which the
	// line names when no counted step is under way.
	named []named

	stop, stopped chan struct{}
	closing       sync.Once
}

type named struct {
	span progress.SpanID
	name string
}

// CounterOptions configure a Counter.
type CounterOptions struct {
	// Now is the clock the time on the line is read from; nil is time.Now.
	Now func() time.Time
}

// NewCounter returns a counter that draws on term once Start is called. The
// time on its line is counted from now.
func NewCounter(term *Terminal, o CounterOptions) *Counter {
	c := &Counter{term: term, now: o.Now}
	if c.now == nil {
		c.now = time.Now
	}
	c.start = c.now()
	return c
}

// Start draws the counter every 100ms, from a second after it was made,
// until Close.
func (c *Counter) Start() {
	c.stop, c.stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(c.stopped)
		defer c.term.recovered()
		tick := time.NewTicker(counterEvery)
		defer tick.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-tick.C:
				c.Draw()
			}
		}
	}()
}

// Draw draws the line as the work stands now, unless the command has run
// for less than a second.
func (c *Counter) Draw() {
	now := c.now()
	if now.Sub(c.start) < counterDelay {
		return
	}
	line := func() string {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.line(now)
	}()
	c.term.draw(line)
}

// Close stops the drawing and takes the line off the terminal. Closing a
// closed Counter does nothing.
func (c *Counter) Close() {
	c.closing.Do(func() {
		if c.stop != nil {
			close(c.stop)
			<-c.stopped
		}
		c.term.close()
	})
}

// Handle counts e in.
func (c *Counter) Handle(e progress.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tally.Add(e)
	if e.Kind != progress.KindCommand && e.Kind != progress.KindStep {
		return
	}
	switch e.Type {
	case progress.TypeStart:
		if e.Flags&progress.Hidden == 0 {
			c.named = append(c.named, named{e.Span, e.Name})
		}
	case progress.TypeEnd:
		for i, n := range c.named {
			if n.span == e.Span {
				c.named = append(c.named[:i], c.named[i+1:]...)
				break
			}
		}
	}
}

// Suspend takes the line off the terminal until Resume.
func (c *Counter) Suspend() { c.term.suspend() }

// Resume lets the line back.
func (c *Counter) Resume() { c.term.resume() }

// line is the line to draw at now. c.mu is held.
func (c *Counter) line(now time.Time) string {
	var segments []string
	for _, root := range c.tally.Roots() {
		if root.Flags&progress.Hidden == 0 {
			segments = append(segments, segment(root))
		}
	}
	if len(segments) == 0 && len(c.named) > 0 {
		segments = append(segments, c.named[len(c.named)-1].name)
	}
	line := strings.Join(segments, " | ")
	if line != "" {
		line += " · "
	}
	return line + elapsed(now.Sub(c.start))
}

// segment says how far one counted step has got.
func segment(n progress.Count) string {
	parts := []string{n.Name}
	if n.Batch != "" {
		parts = append(parts, "batch "+n.Batch)
	}
	parts = append(parts, fmt.Sprintf("%d/%d", n.Done, n.Total))
	for _, count := range []struct {
		n    int
		what string
	}{
		{n.Failed, "failed"},
		{n.Canceled, "canceled"},
		{n.Skipped, "skipped"},
		{n.Running, "running"},
		{n.Queued, "queued"},
	} {
		if count.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count.n, count.what))
		}
	}
	if n.Waits > 0 {
		parts = append(parts, "waiting")
	}
	return strings.Join(parts, " · ")
}

// elapsed reads d as minutes and seconds, or hours, minutes and seconds.
func elapsed(d time.Duration) string {
	s := int(d / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
