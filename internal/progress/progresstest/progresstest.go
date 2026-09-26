// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package progresstest captures the progress events of a test, draws them
// as a tree that does not depend on the order concurrent work happened in,
// and checks that they keep the promises the progress package makes to
// every sink.
package progresstest

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Capture is a sink that keeps every event it is sent. It is safe for
// concurrent use.
type Capture struct {
	// Lines makes the capture ask for lines of output, as a live display
	// does, so that progress.Tee produces them.
	Lines bool

	mu     sync.Mutex
	events []progress.Event
}

// Handle keeps e.
func (c *Capture) Handle(e progress.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

// WantsLines reports c.Lines.
func (c *Capture) WantsLines() bool { return c.Lines }

// Events returns a copy of the events kept so far.
func (c *Capture) Events() []progress.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

// Tree draws the spans of the events kept so far, one line each, indented
// under their parent:
//
//	step reset the machines total=3 limit=8 [fold]: failed (target): 1 of 3 failed
//	  target exe[1-2]: ok
//	    call redfish method=POST path=/redfish/v1/Systems/1: ok
//	  target exe3: failed (transport): {}: no answer
//
// Nothing in it depends on how concurrent work was scheduled. Targets that
// read the same once their own node and host are written {} are folded into
// one line naming them as a node set; siblings are sorted by what they
// read, with numbers in their numeric order; and neither ids, times nor
// lines of output are drawn. A span not ended reads as its state.
func (c *Capture) Tree() string {
	return tree(c.Events())
}

type node struct {
	start, last progress.Event
	ended       bool
	children    []*node
}

func tree(events []progress.Event) string {
	nodes := map[progress.SpanID]*node{}
	var roots []*node
	for _, e := range events {
		switch e.Type {
		case progress.TypeStart:
			n := &node{start: e, last: e}
			nodes[e.Span] = n
			if p := nodes[e.Parent]; e.Parent != 0 && p != nil {
				p.children = append(p.children, n)
			} else {
				roots = append(roots, n)
			}
		case progress.TypeRun, progress.TypeUpdate, progress.TypeEnd:
			if n := nodes[e.Span]; n != nil {
				n.last = e
				n.ended = n.ended || e.Type == progress.TypeEnd
			}
		}
	}
	return strings.Join(blocks(roots, func(s string) string { return s }), "")
}

// foldHere marks where a folded target's names go.
const foldHere = "\x00"

// blocks draws sibling spans, folding the targets among them.
func blocks(siblings []*node, repl func(string) string) []string {
	var out []string
	folded := map[string][]string{}
	for _, n := range siblings {
		if n.start.Kind != progress.KindTarget {
			out = append(out, draw(n, repl))
			continue
		}
		var pairs []string
		// The host first, since it usually holds the node's name.
		for _, own := range []string{n.last.Host, n.last.Node, n.start.Name} {
			if own != "" {
				pairs = append(pairs, own, "{}")
			}
		}
		own := strings.NewReplacer(pairs...)
		block := draw(n, func(s string) string { return own.Replace(repl(s)) })
		folded[block] = append(folded[block], cmp.Or(n.last.Node, n.start.Name))
	}
	for block, names := range folded {
		out = append(out, strings.Replace(block, foldHere, fold(names), 1))
	}
	slices.SortFunc(out, natural)
	return out
}

// draw draws one span and those under it.
func draw(n *node, repl func(string) string) string {
	e := n.last
	var b strings.Builder
	b.WriteString(n.start.Kind.String())
	b.WriteByte(' ')
	if n.start.Kind == progress.KindTarget {
		b.WriteString(foldHere)
	} else {
		b.WriteString(repl(n.start.Name))
		field(&b, "node", repl(e.Node))
		field(&b, "host", repl(e.Host))
	}
	field(&b, "role", e.Role)
	field(&b, "batch", e.Batch)
	if e.Total != 0 {
		field(&b, "total", fmt.Sprint(e.Total))
	}
	if e.Limit != 0 {
		field(&b, "limit", fmt.Sprint(e.Limit))
	}
	field(&b, "method", e.Method)
	field(&b, "path", repl(e.Path))
	if e.HTTPStatus != 0 {
		field(&b, "http", fmt.Sprint(e.HTTPStatus))
	}
	field(&b, "cache", e.Cache)
	field(&b, "source", e.Source)
	if e.Timeout != 0 {
		field(&b, "timeout", e.Timeout.String())
	}
	if e.Exit != nil {
		field(&b, "exit", fmt.Sprint(*e.Exit))
	}
	field(&b, "message", repl(e.Message))
	if e.Flags != 0 {
		fmt.Fprintf(&b, " [%s]", e.Flags)
	}
	if !n.ended {
		fmt.Fprintf(&b, ": %s\n", e.State)
	} else {
		fmt.Fprintf(&b, ": %s", e.Status)
		if e.Class != progress.ClassNone {
			fmt.Fprintf(&b, " (%s)", e.Class)
		}
		if e.Err != "" {
			fmt.Fprintf(&b, ": %s", repl(e.Err))
		}
		b.WriteByte('\n')
	}
	for _, child := range blocks(n.children, repl) {
		for line := range strings.SplitAfterSeq(child, "\n") {
			if line != "" {
				b.WriteString("  " + line)
			}
		}
	}
	return b.String()
}

func field(b *strings.Builder, key, value string) {
	if value != "" {
		fmt.Fprintf(b, " %s=%s", key, value)
	}
}

// fold names targets as a node set, or lists them when one of the names
// does not read as itself in one, as "port 10" would not.
func fold(names []string) string {
	set := nodeset.New()
	for _, name := range names {
		one, err := nodeset.Parse(name)
		if err != nil || one.String() != name {
			slices.SortFunc(names, natural)
			return strings.Join(names, ",")
		}
		_ = set.Add(name)
	}
	return set.String()
}

// natural orders strings with the numbers in them compared by value, so
// that batch 2/10 comes before batch 10/10.
func natural(a, b string) int {
	for a != "" && b != "" {
		da, db := digits(a), digits(b)
		if da > 0 && db > 0 {
			na, nb := strings.TrimLeft(a[:da], "0"), strings.TrimLeft(b[:db], "0")
			if c := cmp.Or(cmp.Compare(len(na), len(nb)), strings.Compare(na, nb)); c != 0 {
				return c
			}
			a, b = a[da:], b[db:]
			continue
		}
		if a[0] != b[0] {
			return cmp.Compare(a[0], b[0])
		}
		a, b = a[1:], b[1:]
	}
	return cmp.Compare(len(a), len(b))
}

func digits(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

// Check reports, as errors of t, every promise the events break. Call it
// once the Bus is closed, when every span has ended:
//
//  1. Seq runs from 1 with no gaps. Every span starts once and ends once,
//     and no span id is zero or used twice.
//  2. A span starts under a parent that has started and not ended, and
//     ends after every span under it. Its Run, Update and Line events come
//     between its Start and its End, and a Run only follows a queued Start.
//  3. The work a step knows of is announced up front. The targets of a
//     Fold step or a batch, and the batches of a step, all start queued
//     before the first of them runs. When a Fold step or a batch ends,
//     its targets that ended, however they ended, are its Total; a batch
//     or a Fold step that was skipped or canceled before any target of
//     its own started counts as its Total. No count ever exceeds its
//     Total, and no Total shrinks, so a counter never goes backwards and
//     always reaches its total, even after an interrupt.
//  4. No more targets run at once below a span than its Limit.
//  5. Every Suspend is followed by one Resume.
//  6. No text holds anything output.EscapeCell would escape, and none is
//     longer than its bound. Hidden and ShowLines are passed down.
func Check(t testing.TB, events []progress.Event) {
	t.Helper()
	problems := violations(events)
	for i, p := range problems {
		if i == 20 {
			t.Errorf("progress: and %d more", len(problems)-i)
			break
		}
		t.Errorf("progress: %s", p)
	}
}

type span struct {
	e       progress.Event
	parent  *span
	state   progress.State
	total   int
	open    int
	running int
	// ran is set once a target or a batch below this Fold step or batch
	// has run.
	ran bool
}

func (s *span) String() string {
	return fmt.Sprintf("%s %q (event %d)", s.e.Kind, s.e.Name, s.e.Seq)
}

// counts reports whether s counts the targets below it against its Total.
func (s *span) counts() bool {
	return s.e.Kind == progress.KindBatch || s.e.Kind == progress.KindStep && s.e.Flags&progress.Fold != 0
}

func violations(events []progress.Event) []string {
	var problems []string
	bad := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	spans := map[progress.SpanID]*span{}
	var order []*span
	suspended := 0
	// The counts are the ones a display shows, kept by the display's
	// own rules.
	var tally progress.Tally
	// overCounted reports every span above s whose count has passed its
	// Total.
	overCounted := func(s *span) {
		for p := s.parent; p != nil; p = p.parent {
			if c, ok := tally.Count(p.e.Span); ok && c.Done > c.Total {
				bad("%s counts %d targets, more than its Total of %d", p, c.Done, c.Total)
			}
		}
	}
	// run counts a target in as running below every span with a Limit.
	run := func(s *span, n int) {
		for p := s.parent; p != nil; p = p.parent {
			if p.e.Limit > 0 {
				p.running += n
				if p.running > p.e.Limit {
					bad("%s runs %d targets at once, more than its Limit of %d", p, p.running, p.e.Limit)
				}
			}
		}
	}

	for i, e := range events {
		if want := uint64(i + 1); e.Seq != want {
			bad("event %d has Seq %d", want, e.Seq)
		}
		checkText(e, bad)
		ended, counted := tally.Add(e)

		switch e.Type {
		case progress.TypeSuspend:
			suspended++
			continue
		case progress.TypeResume:
			suspended--
			if suspended < 0 {
				bad("event %d resumes what was not suspended", e.Seq)
				suspended = 0
			}
			continue
		case progress.TypeStart:
			if e.Span == 0 {
				bad("event %d starts a span with id 0", e.Seq)
			}
			if _, ok := spans[e.Span]; ok {
				bad("event %d starts span %s a second time", e.Seq, e.Span)
				continue
			}
			s := &span{e: e, state: e.State, total: e.Total}
			if e.Kind < progress.KindCommand || e.Kind > progress.KindWait {
				bad("%s has no valid kind", s)
			}
			if e.State != progress.StateQueued && e.State != progress.StateRunning {
				bad("%s starts %s", s, e.State)
			}
			if e.Parent != 0 {
				p := spans[e.Parent]
				switch {
				case p == nil:
					bad("%s starts under span %s, which has not started", s, e.Parent)
				case p.state == progress.StateEnded:
					bad("%s starts under %s, which has ended", s, p)
				}
				if p != nil {
					s.parent = p
					p.open++
					if missing := p.e.Flags & (progress.Hidden | progress.ShowLines) &^ e.Flags; missing != 0 {
						bad("%s lacks the %s of its parent", s, missing)
					}
					if p.counts() && (e.Kind == progress.KindTarget || e.Kind == progress.KindBatch) {
						if e.State != progress.StateQueued {
							bad("%s is not queued under %s", s, p)
						}
						if p.ran {
							bad("%s is announced under %s after another has run", s, p)
						}
					}
				}
			}
			spans[e.Span] = s
			order = append(order, s)
			if e.Kind == progress.KindTarget && e.State == progress.StateRunning {
				run(s, 1)
			}
			continue
		}

		s := spans[e.Span]
		switch {
		case s == nil:
			bad("event %d (%s) is about span %s, which has not started", e.Seq, e.Type, e.Span)
			continue
		case s.state == progress.StateEnded:
			bad("event %d (%s) comes after %s ended", e.Seq, e.Type, s)
			continue
		}
		if e.Total < s.total {
			bad("event %d lowers the Total of %s from %d to %d", e.Seq, s, s.total, e.Total)
		}
		s.total = max(s.total, e.Total)

		switch e.Type {
		case progress.TypeRun:
			if s.state != progress.StateQueued {
				bad("event %d runs %s, which was not queued", e.Seq, s)
			}
			s.state = progress.StateRunning
			if p := s.parent; p != nil && p.counts() && (s.e.Kind == progress.KindTarget || s.e.Kind == progress.KindBatch) {
				p.ran = true
			}
			if s.e.Kind == progress.KindTarget {
				run(s, 1)
			}
		case progress.TypeUpdate:
		case progress.TypeLine:
			if e.Stream != progress.Stdout && e.Stream != progress.Stderr {
				bad("event %d is a line of no stream", e.Seq)
			}
		case progress.TypeEnd:
			if s.open > 0 {
				bad("%s ends before the %d spans under it", s, s.open)
			}
			if e.Status < progress.StatusOK || e.Status > progress.StatusSkipped {
				bad("%s ends with no status", s)
			}
			if s.e.Kind == progress.KindTarget && s.state == progress.StateRunning {
				run(s, -1)
			}
			s.state = progress.StateEnded
			if s.parent != nil {
				s.parent.open--
			}
			leftOut := counted && ended.Targets == 0 &&
				(e.Status == progress.StatusSkipped || e.Status == progress.StatusCanceled)
			if counted && !leftOut && ended.Done != ended.Total {
				bad("%s ends with %d of its Total of %d targets", s, ended.Done, ended.Total)
			}
			if s.e.Kind == progress.KindTarget || leftOut {
				overCounted(s)
			}
		default:
			bad("event %d has no valid type", e.Seq)
		}
	}

	for _, s := range order {
		if s.state != progress.StateEnded {
			bad("%s never ends", s)
		}
	}
	if suspended > 0 {
		bad("%d suspensions are never resumed", suspended)
	}
	return problems
}

// checkText reports text that could reach a terminal unescaped, or that
// exceeds its bound.
func checkText(e progress.Event, bad func(string, ...any)) {
	for _, f := range []struct {
		name, text string
		max        int
	}{
		{"name", e.Name, progress.MaxField},
		{"node", e.Node, 0},
		{"host", e.Host, progress.MaxField},
		{"role", e.Role, progress.MaxField},
		{"batch", e.Batch, progress.MaxField},
		{"message", e.Message, progress.MaxField},
		{"method", e.Method, progress.MaxField},
		{"path", e.Path, progress.MaxField},
		{"cache", e.Cache, progress.MaxField},
		{"source", e.Source, progress.MaxField},
		{"error", e.Err, progress.MaxErr},
		{"text", e.Text, progress.MaxText},
	} {
		if output.EscapeCell(f.text) != f.text {
			bad("event %d has a %s that is not escaped: %q", e.Seq, f.name, f.text)
		}
		if f.max > 0 && len(f.text) > f.max {
			bad("event %d has a %s of %d bytes, more than %d", e.Seq, f.name, len(f.text), f.max)
		}
	}
}
