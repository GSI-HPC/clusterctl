// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display

import (
	"cmp"
	"fmt"
	"sync"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// Summary is the one line a display leaves behind once the command has
// ended: how the command ended, how long it ran, and how many of the things
// it worked on ended how.
//
//	provision reinstall: failed in 18m03s: 478 ok, 2 failed
//
// How the command ended comes first, since the counts need not say it: a
// command that fails in a step that counts nothing, or stops before the
// steps that would have counted the rest, can have counted nothing but ok.
// Each node, or whatever else a target names, counts once, as the worst of
// the ways its targets ended below the counted steps: failed, then
// canceled, then skipped, then ok, so that a node reset after its boot
// override was set is one node, not two. A batch left out, after one that
// failed or for an interrupt, counts as its Total. A command that counted
// no targets says only how it ended. The line says nothing of why a target
// failed: the command's error says that, once, after it.
//
// A Summary is a progress.Sink; its methods are safe for concurrent use.
type Summary struct {
	mu    sync.Mutex
	tally progress.Tally
	// command is the span of the command, the root of the tree.
	command    progress.SpanID
	name       string
	start, end time.Time
	status     progress.Status
	// roots are the counted steps no counted span is above, and under
	// says which of those each open span is under.
	roots map[progress.SpanID]*outcomes
	under map[progress.SpanID]progress.SpanID
	// nodes are the worst way each node's targets ended, and left the
	// Totals of what was left out.
	nodes map[string]progress.Status
	left  outcomes
}

// outcomes counts targets by how they ended.
type outcomes struct{ failed, canceled, skipped int }

// Handle counts e in.
func (s *Summary) Handle(e progress.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roots == nil {
		s.roots = map[progress.SpanID]*outcomes{}
		s.under = map[progress.SpanID]progress.SpanID{}
		s.nodes = map[string]progress.Status{}
	}
	count, counted := s.tally.Add(e)
	switch e.Type {
	case progress.TypeStart:
		s.begin(e)
	case progress.TypeEnd:
		s.finish(e, count, counted)
	}
}

func (s *Summary) begin(e progress.Event) {
	if e.Kind == progress.KindCommand && s.command == 0 {
		s.command, s.name, s.start = e.Span, e.Name, e.Time
	}
	root := s.under[e.Parent]
	counts := e.Kind == progress.KindBatch || e.Kind == progress.KindStep && e.Flags&progress.Fold != 0
	// A hidden step is not shown, and neither is what it counts, as the
	// counter does not show it.
	if root == 0 && counts && e.Flags&progress.Hidden == 0 {
		root = e.Span
		s.roots[root] = &outcomes{}
	}
	if root != 0 {
		s.under[e.Span] = root
	}
}

func (s *Summary) finish(e progress.Event, count progress.Count, counted bool) {
	if e.Span == s.command {
		s.end, s.status = e.Time, e.Status
	}
	root, ok := s.under[e.Span]
	if !ok {
		return
	}
	delete(s.under, e.Span)
	if e.Kind == progress.KindTarget {
		node := cmp.Or(e.Node, e.Name)
		if was, seen := s.nodes[node]; !seen || worse(e.Status, was) {
			s.nodes[node] = e.Status
		}
		s.roots[root].add(e.Status, 1)
	}
	if root != e.Span || !counted {
		return
	}
	// What the root counted that no target ended as was left out: the
	// Total of a batch or a Fold step below it that never ran. A root
	// left out whole counts nothing of its own, as Tally has it.
	seen := s.roots[root]
	s.left.failed += count.Failed - seen.failed
	s.left.canceled += count.Canceled - seen.canceled
	s.left.skipped += count.Skipped - seen.skipped
	delete(s.roots, root)
}

func (o *outcomes) add(status progress.Status, n int) {
	switch status {
	case progress.StatusFailed:
		o.failed += n
	case progress.StatusCanceled:
		o.canceled += n
	case progress.StatusSkipped:
		o.skipped += n
	}
}

// worse reports whether a target that ended a says more about how its node
// fared than one that ended b.
func worse(a, b progress.Status) bool {
	rank := func(s progress.Status) int {
		switch s {
		case progress.StatusFailed:
			return 3
		case progress.StatusCanceled:
			return 2
		case progress.StatusSkipped:
			return 1
		}
		return 0
	}
	return rank(a) > rank(b)
}

// Line returns the summary once the command has ended, if it is worth a
// line: when the command ran for a second or longer, as long as a display
// takes to show itself, so that a command done within its first second
// leaves standard error as it would without a display, failed targets or
// not; its error says what failed. Otherwise, and before the command has
// ended, it is "".
func (s *Summary) Line() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.command == 0 || s.end.IsZero() {
		return ""
	}
	var ok int
	all := s.left
	for _, status := range s.nodes {
		if status == progress.StatusOK {
			ok++
		}
		all.add(status, 1)
	}
	d := s.end.Sub(s.start)
	if d < time.Second {
		return ""
	}
	line := fmt.Sprintf("%s: %s in %s", s.name, word(s.status), took(d))
	if ok+all.failed+all.canceled+all.skipped == 0 {
		return line
	}
	return line + ": " + tallied(ok, all.failed, all.canceled, all.skipped)
}

// took reads a length of time the way a person says it: tenths of a second
// below ten seconds, then seconds, minutes and seconds, and hours, minutes
// and seconds.
func took(d time.Duration) string {
	d = max(0, d)
	s := int(d / time.Second)
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", s)
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm%02ds", s/3600, s/60%60, s%60)
}
