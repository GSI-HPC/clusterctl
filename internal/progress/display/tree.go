// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

const (
	// treeEvery is how often the tree is drawn at most.
	treeEvery = 100 * time.Millisecond
	// treeColumns and treeRows are the smallest terminal the tree is
	// drawn on. On a smaller one the counter's line is drawn instead.
	treeColumns, treeRows = 40, 8
	// treeHeight is the fewest rows the tree may take; on a terminal of
	// more than three times as many it takes up to a third of them, so
	// that what the command printed stays in view.
	treeHeight = 6
	// revealAfter is how long a hidden span runs before it is drawn.
	revealAfter = time.Second
	// quick is how soon a span that ends well, with nothing drawn below
	// it, is done without ever being drawn: a row for it would only
	// flicker.
	quick = 100 * time.Millisecond
)

// glyphs are the marks a display draws with.
type glyphs struct {
	ok, failed, canceled, skipped, running, more string
	// sep splits the parts of a row, and path the names of a step and
	// the steps it is part of.
	sep, path string
}

var (
	unicodeGlyphs = glyphs{ok: "✓", failed: "✗", canceled: "⊘", skipped: "–", running: "▸", more: "…", sep: " · ", path: " › "}
	// asciiGlyphs are for a terminal whose locale is not UTF-8, which
	// would show the others as garbage.
	asciiGlyphs = glyphs{ok: "+", failed: "x", canceled: "~", skipped: "-", running: ">", more: "...", sep: " - ", path: " > "}
)

func glyphsFor(ascii bool) glyphs {
	if ascii {
		return asciiGlyphs
	}
	return unicodeGlyphs
}

// Tree is the live display of a terminal: a few rows at its bottom, the
// region, that show the work under way as a tree, redrawn as it goes on,
// and a line above them for each step once it has finished:
//
//	✓ configuring the network boot  2.3s
//	provision reinstall · 4:12
//	  setting the machines to boot from the network once  312/480 · 1 failed · 8 running · 159 queued
//	    ✗ exe0007  transport: {}: dial tcp: i/o timeout
//	    ▸ exe0313  4s  PATCH /redfish/v1/Systems/1
//	    ▸ exe0314  3s  GET /redfish/v1/Systems/1
//	    … 6 more running
//	    ✓ exe[0001-0006,0008-0312]
//
// The command heads the region, and each step, batch and wait under way
// has a row, indented under the span it is part of; a step says how many
// of its targets are done, of how many, and how they stand, as a
// progress.Tally counts them. The targets of a step are folded: those that
// ended well into one node set, those that failed into one row for each
// class and error they share, with a target's own name read as {}, those
// interrupted and those left out into one row each. Those still queued are
// only counted, and those running are listed, the longest running first,
// with how long they have run, against the bound of the request they wait
// for when it has one, and what that request is, or the last line of
// output for a step that shows lines. A pause counts down.
//
// A hidden span is drawn only once it has run for a second, and a line is
// left for it only when it failed or took that long. A span that ends well
// within a tenth of a second, with nothing drawn below it, is done without
// a row or a line; one that failed keeps its line however quickly it did,
// and so does one that had something drawn below it. A step with no name
// is no row: what is below it is drawn in its place.
//
// The region is at most a third of the terminal's rows, and six rows on a
// smaller one; rows that do not fit are cut, the running targets first,
// each list down to a row that says how many it leaves out, and every row
// is cut to the width, so that none wraps. The tree is drawn at most ten
// times a second, from a second after it was made: a command done by then
// never shows one, nor the lines of its steps. On a terminal of fewer than
// 8 rows or 40 columns, as it says at each frame, the counter's line is
// drawn instead. Once the command has been interrupted, its row says so,
// with how many targets will stop and how many will not start.
//
// A Tree is a progress.Sink, a progress.LineSink and a progress.Suspender.
// Its methods are safe for concurrent use.
type Tree struct {
	term        *Terminal
	now         func() time.Time
	start       time.Time
	g           glyphs
	interrupted <-chan struct{}
	// counter is drawn in the tree's place on a terminal too small for
	// it.
	counter *Counter

	mu    sync.Mutex
	tally progress.Tally
	spans map[progress.SpanID]*treeSpan
	// roots are the open spans started under none: the command.
	roots []*treeSpan
	// shown says the tree has been drawn, or would have been: the lines
	// of the steps that finished may go out from then on.
	shown bool
	// lines are the lines above the region not yet written, each ended by
	// a newline.
	lines strings.Builder

	stop, stopped chan struct{}
	closing       sync.Once
}

// TreeOptions configure a Tree.
type TreeOptions struct {
	// Now is the clock the times of the tree are read from; nil is
	// time.Now. The Bus's clock should be the same.
	Now func() time.Time
	// ASCII draws with ASCII marks alone, for a terminal whose locale is
	// not UTF-8.
	ASCII bool
	// Interrupted is closed once the command has been interrupted; nil is
	// a command that never is.
	Interrupted <-chan struct{}
}

// treeSpan is an open span as the tree knows it.
type treeSpan struct {
	id     progress.SpanID
	kind   progress.Kind
	name   string
	flags  progress.Flags
	state  progress.State
	fields progress.Fields
	parent *treeSpan
	// children are the open spans started under this one, in the order
	// they started.
	children []*treeSpan
	// started is when the span started, and ran when it started running.
	started, ran time.Time
	// line is the last line of output of the work below a target.
	line string
	// drawsBelow says a span that is drawn in a row of its own, or
	// folded into one, started under this one: it keeps its line however
	// quickly it ended.
	drawsBelow bool
	// ended folds the targets below that have ended, nil for none.
	ended *folded
	// path names a step from the one under the command down, for its
	// line; spans that are not steps, and those under a target, which get
	// no line, pass their parent's on.
	path string
	// underTarget says the span is part of a target's work, which is
	// drawn in the target's row.
	underTarget bool
}

// NewTree returns a tree that draws on term once Start is called. The time
// on its first row is counted from now.
func NewTree(term *Terminal, o TreeOptions) *Tree {
	t := &Tree{term: term, now: o.Now, g: glyphsFor(o.ASCII), interrupted: o.Interrupted, spans: map[progress.SpanID]*treeSpan{}}
	if t.now == nil {
		t.now = time.Now
	}
	t.counter = NewCounter(term, CounterOptions{Now: t.now, ASCII: o.ASCII})
	t.start = t.now()
	term.mu.Lock()
	term.held = t.take
	term.mu.Unlock()
	return t
}

// Start draws the tree every 100ms, from a second after it was made, until
// Close.
func (t *Tree) Start() {
	t.stop, t.stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(t.stopped)
		defer t.term.recovered()
		tick := time.NewTicker(treeEvery)
		defer tick.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-tick.C:
				t.Draw()
			}
		}
	}()
}

// Draw draws a frame as the work stands now, with the lines of the steps
// that finished above it, unless the command has run for less than a
// second. The terminal writes nothing for a frame that reads as the one
// before.
func (t *Tree) Draw() {
	now := t.now()
	if now.Sub(t.start) < counterDelay {
		return
	}
	t.mu.Lock()
	t.shown = true
	t.mu.Unlock()
	width, height := t.term.dims()
	if width < treeColumns || height < treeRows {
		t.counter.Draw()
		return
	}
	rows := func() []string {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.frame(now, max(treeHeight, height/3))
	}()
	t.term.draw(rows)
}

// Close stops the drawing, takes the region off the terminal and writes
// the lines not yet written, if the tree was ever drawn. Closing a closed
// Tree does nothing.
func (t *Tree) Close() {
	t.closing.Do(func() {
		if t.stop != nil {
			close(t.stop)
			<-t.stopped
		}
		t.term.close()
	})
}

// Suspend takes the region off the terminal, once the lines not yet
// written are, until Resume.
func (t *Tree) Suspend() { t.term.suspend() }

// Resume lets the region back.
func (t *Tree) Resume() { t.term.resume() }

// WantsLines asks for the lines of output of a step that shows them, the
// last of which a running target's row shows.
func (t *Tree) WantsLines() bool { return true }

// take returns the lines not yet written, and forgets them, once the tree
// has been drawn. Until then the lines of the steps that finished wait for
// the first frame, and a tree never drawn never writes them; but once
// something else is written on the terminal, output or a question, they are
// dropped, as they would be had the command ended then: written later,
// they would stand below what came after them. The terminal's lock is
// held.
func (t *Tree) take() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.shown {
		t.lines.Reset()
		return ""
	}
	text := t.lines.String()
	t.lines.Reset()
	return text
}

// Handle keeps what e changes.
func (t *Tree) Handle(e progress.Event) {
	t.counter.Handle(e)
	t.mu.Lock()
	defer t.mu.Unlock()
	count, counted := t.tally.Add(e)
	s := t.spans[e.Span]
	switch e.Type {
	case progress.TypeStart:
		t.begin(e)
	case progress.TypeRun:
		if s != nil {
			s.state, s.fields, s.ran = e.State, e.Fields, e.Time
		}
	case progress.TypeUpdate:
		if s != nil {
			s.fields = e.Fields
		}
	case progress.TypeLine:
		for ; s != nil; s = s.parent {
			if s.kind == progress.KindTarget {
				s.line = e.Text
				break
			}
		}
	case progress.TypeEnd:
		if s != nil {
			t.end(e, s, count, counted)
		}
	}
}

func (t *Tree) begin(e progress.Event) {
	if _, again := t.spans[e.Span]; again {
		return
	}
	s := &treeSpan{id: e.Span, kind: e.Kind, name: e.Name, flags: e.Flags, state: e.State, fields: e.Fields, started: e.Time}
	if e.State == progress.StateRunning {
		s.ran = e.Time
	}
	t.spans[e.Span] = s
	p := t.spans[e.Parent]
	if e.Parent == 0 || p == nil {
		t.roots = append(t.roots, s)
		return
	}
	s.parent = p
	p.children = append(p.children, s)
	s.underTarget = p.kind == progress.KindTarget || p.underTarget
	if e.Kind != progress.KindCall && e.Flags&progress.Hidden == 0 {
		p.drawsBelow = true
	}
	s.path = p.path
	if e.Kind == progress.KindStep && e.Name != "" && !s.underTarget {
		if s.path != "" {
			s.path += t.g.path
		}
		s.path += e.Name
	}
}

func (t *Tree) end(e progress.Event, s *treeSpan, count progress.Count, counted bool) {
	delete(t.spans, e.Span)
	s.fields = e.Fields
	p := s.parent
	if p == nil {
		t.roots = slices.DeleteFunc(t.roots, func(r *treeSpan) bool { return r == s })
	} else {
		p.children = slices.DeleteFunc(p.children, func(c *treeSpan) bool { return c == s })
	}
	switch s.kind {
	case progress.KindTarget:
		if p != nil {
			p.fold().add(s, e)
		}
	case progress.KindBatch:
		// A batch is folded into its step the way its targets are, so
		// that what failed in a batch stays in view once it is over.
		if p == nil {
			break
		}
		switch {
		case s.ended != nil:
			p.fold().merge(s.ended)
		case s.ran.IsZero() && e.Status == progress.StatusSkipped:
			p.fold().skip(s.fields.Node, e.Err)
		case s.ran.IsZero() && e.Status == progress.StatusCanceled:
			p.fold().canceled.add(s.fields.Node)
		}
	case progress.KindStep:
		t.finished(e, s, count, counted)
	}
}

// finished leaves the line of a step that ended, with the targets of it
// that failed below it, unless the step is one the tree does not show:
// one with no name, one under a target, a hidden one that ended well
// within a second, or one that ended well at once with nothing drawn below
// it. t.mu is held.
func (t *Tree) finished(e progress.Event, s *treeSpan, count progress.Count, counted bool) {
	if e.Name == "" || s.underTarget {
		return
	}
	ran := !s.ran.IsZero()
	d := e.Time.Sub(s.ran)
	if e.Status == progress.StatusOK {
		if s.flags&progress.Hidden != 0 && (!ran || d < revealAfter) || d < quick && !s.drawsBelow {
			return
		}
	}
	text := t.mark(e.Status) + " " + s.path
	switch {
	case !ran:
		text += "  " + outcome(e)
	case counted:
		text += "  " + took(d) + "  " + ended(count)
	default:
		text += "  " + took(d)
	}
	t.lines.WriteString(text + "\n")
	if s.ended != nil {
		for _, f := range s.ended.failures {
			fmt.Fprintf(&t.lines, "  %s %s  %s\n", t.g.failed, f.names.String(), f.text)
		}
	}
}

// mark is the mark of a span that ended with status.
func (t *Tree) mark(status progress.Status) string {
	switch status {
	case progress.StatusFailed:
		return t.g.failed
	case progress.StatusCanceled:
		return t.g.canceled
	case progress.StatusSkipped:
		return t.g.skipped
	}
	return t.g.ok
}

// folded are the targets below a span that have ended, folded by how they
// ended.
type folded struct {
	ok, canceled, skipped names
	// failures are grouped by what their class and error read with each
	// target's own name as {}, in the order the first of each failed.
	failures []*failure
	// reason is why the targets were left out, when they all say the
	// same; reasons counts the reasons they gave.
	reason  string
	reasons int
}

type failure struct {
	text  string
	names names
}

func (s *treeSpan) fold() *folded {
	if s.ended == nil {
		s.ended = &folded{}
	}
	return s.ended
}

// add folds in a target that ended as e says.
func (f *folded) add(s *treeSpan, e progress.Event) {
	name := cmp.Or(e.Node, s.name)
	switch e.Status {
	case progress.StatusOK:
		f.ok.add(name)
	case progress.StatusFailed:
		f.fail(failureText(s, e), &names{}).add(name)
	case progress.StatusCanceled:
		f.canceled.add(name)
	case progress.StatusSkipped:
		f.skip(name, e.Err)
	}
}

// fail adds some to the targets that failed as text says, a new row of
// failures when none failed so before, and returns the names of that row.
func (f *folded) fail(text string, some *names) *names {
	for _, g := range f.failures {
		if g.text == text {
			g.names.merge(some)
			return &g.names
		}
	}
	g := &failure{text: text}
	g.names.merge(some)
	f.failures = append(f.failures, g)
	return &g.names
}

func (f *folded) skip(name, reason string) {
	f.skipped.add(name)
	f.reasons++
	if f.reasons == 1 {
		f.reason = reason
	} else if reason != f.reason {
		f.reason = ""
	}
}

// merge folds in what a batch folded.
func (f *folded) merge(o *folded) {
	f.ok.merge(&o.ok)
	f.canceled.merge(&o.canceled)
	for _, g := range o.failures {
		f.fail(g.text, &g.names)
	}
	if o.reasons > 0 {
		f.skipped.merge(&o.skipped)
		f.reasons += o.reasons
		if f.reasons == o.reasons {
			f.reason = o.reason
		} else if o.reason != f.reason {
			f.reason = ""
		}
	}
}

// addresses matches an IP address, with its port, as an error that names
// what it dialled has it: 10.0.0.7:443, [fe80::1]:623.
var addresses = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b|\[[0-9A-Fa-f:.]+\](?::\d+)?`)

// failureText is how a target failed: its class and its error, with its
// own name, and its host's, and any address, read as {}, so that the
// targets that failed alike share it, the processors that refused the
// connection to their own addresses among them.
func failureText(s *treeSpan, e progress.Event) string {
	if e.Class == progress.ClassNone && e.Err == "" {
		return "failed"
	}
	var pairs []string
	// The host first, since it usually holds the node's name.
	for _, own := range []string{e.Host, e.Node, s.name} {
		if own != "" {
			pairs = append(pairs, own, "{}")
		}
	}
	text := e.Class.String()
	if e.Err != "" {
		text += ": " + addresses.ReplaceAllString(strings.NewReplacer(pairs...).Replace(e.Err), "{}")
	}
	return text
}

// names are the names of targets, read as a node set, with any that do not
// read as themselves in one, as "port 10" would not, listed after it.
type names struct {
	set   *nodeset.NodeSet
	other []string
	// text is what the names read, when it is not stale.
	text  string
	stale bool
}

func (n *names) add(name string) {
	if one, err := nodeset.Parse(name); err == nil && one.String() == name {
		if n.set == nil {
			n.set = nodeset.New()
		}
		_ = n.set.Add(name)
	} else {
		n.other = append(n.other, name)
	}
	n.stale = true
}

func (n *names) merge(o *names) {
	if o.set != nil {
		if n.set == nil {
			n.set = nodeset.New()
		}
		n.set = n.set.Union(o.set)
	}
	n.other = append(n.other, o.other...)
	n.stale = true
}

func (n *names) len() int {
	size := len(n.other)
	if n.set != nil {
		size += n.set.Len()
	}
	return size
}

func (n *names) String() string {
	if n.stale {
		var parts []string
		if n.set != nil && !n.set.IsEmpty() {
			parts = append(parts, n.set.String())
		}
		n.text = strings.Join(append(parts, n.other...), ",")
		n.stale = false
	}
	return n.text
}

// A frame is a list of parts, each a row or a list of rows that may be cut
// down to fit the region.
type part struct {
	rows []string
	// cut, for a list, is the row that says how many of its rows are left
	// out, given those left out; nil for a row that is never cut.
	cut func(left []int) string
	// weights are what each row of a list stands for, the targets of a
	// failure; one each when nil.
	weights []int
	// rank orders the lists, the first cut first; keep is how many of its
	// rows are kept.
	rank, keep int
}

// cost is how many rows the part takes with keep of its rows kept.
func (p *part) cost() int {
	if p.cut == nil || p.keep == len(p.rows) {
		return len(p.rows)
	}
	return p.keep + 1
}

type frame struct {
	t     *Tree
	now   time.Time
	parts []*part
}

// frame draws the region as the work stands at now, in at most height
// rows. t.mu is held.
func (t *Tree) frame(now time.Time, height int) []string {
	f := &frame{t: t, now: now}
	for _, root := range t.roots {
		f.span(root, 0)
	}
	return fit(f.parts, height, t.g)
}

// fit cuts the lists of parts, the running targets first and then the
// failures, until they fit in height rows, each list in turn giving up its
// last row, and cuts the last rows off if the rows that are never cut do
// not fit either.
func fit(parts []*part, height int, g glyphs) []string {
	used := 0
	for _, p := range parts {
		p.keep = len(p.rows)
		used += len(p.rows)
	}
	for rank := 0; rank < 2 && used > height; rank++ {
		var lists []*part
		for _, p := range parts {
			if p.cut != nil && p.rank == rank && len(p.rows) > 0 {
				lists = append(lists, p)
			}
		}
		for _, p := range lists {
			used -= p.cost()
			p.keep = 0
			used += p.cost()
		}
		// Rows are given back one list at a time, so that lists side by
		// side share what there is.
		for gave := true; gave; {
			gave = false
			for _, p := range lists {
				if p.keep == len(p.rows) {
					continue
				}
				extra := 1
				if p.keep+1 == len(p.rows) {
					extra = 0
				}
				if used+extra <= height {
					p.keep++
					used += extra
					gave = true
				}
			}
		}
	}
	var rows []string
	for _, p := range parts {
		rows = append(rows, p.rows[:p.keep]...)
		if p.keep < len(p.rows) {
			left := make([]int, 0, len(p.rows)-p.keep)
			for i := p.keep; i < len(p.rows); i++ {
				w := 1
				if p.weights != nil {
					w = p.weights[i]
				}
				left = append(left, w)
			}
			rows = append(rows, p.cut(left))
		}
	}
	if len(rows) > height {
		rows = append(rows[:height-1], g.more)
	}
	return rows
}

func (f *frame) row(depth int, text string) {
	f.parts = append(f.parts, &part{rows: []string{indent(depth) + text}})
}

func indent(depth int) string { return strings.Repeat("  ", depth) }

// drawn reports whether s gets a row of its own now: a span that has not
// started running gets none; a hidden one gets one once it has run for a
// second, and one with nothing drawn below it once it has run for a tenth.
func (f *frame) drawn(s *treeSpan) bool {
	if s.state != progress.StateRunning {
		return false
	}
	age := f.now.Sub(s.ran)
	if s.flags&progress.Hidden != 0 && age < revealAfter {
		return false
	}
	return s.drawsBelow || age >= quick
}

// span draws the rows of s and those below it, s at depth.
func (f *frame) span(s *treeSpan, depth int) {
	g := f.t.g
	switch s.kind {
	case progress.KindCommand:
		f.row(depth, f.header(s))
		f.below(s, depth+1)
	case progress.KindStep, progress.KindBatch:
		if !f.drawn(s) {
			return
		}
		if s.kind == progress.KindStep && s.name == "" {
			f.below(s, depth)
			return
		}
		text := s.name
		if s.kind == progress.KindBatch {
			text = "batch " + text
		}
		if count, ok := f.t.tally.Count(s.id); ok {
			text += "  " + standingOf(count, g)
			if c := f.call(s); c != nil {
				text += "  " + f.took(s, c) + "  " + describe(c, nil)
			}
		} else {
			c := f.call(s)
			text += "  " + f.took(s, c)
			if c != nil {
				text += "  " + describe(c, nil)
			}
		}
		f.row(depth, text)
		f.below(s, depth+1)
	case progress.KindWait:
		if !f.drawn(s) {
			return
		}
		text := s.name + "  "
		if s.fields.Timeout > 0 {
			left := max(0, s.fields.Timeout-f.now.Sub(s.ran))
			text += seconds(left+time.Second-1) + " left"
		} else {
			text += seconds(f.now.Sub(s.ran))
		}
		if s.fields.Message != "" {
			text += "  " + s.fields.Message
		}
		f.row(depth, text)
	case progress.KindCall:
		// A call is drawn in the row of the step or the target it is
		// made for; only one made for the command has a row of its own.
		if s.parent != nil && s.parent.kind != progress.KindCommand || !f.drawn(s) {
			return
		}
		f.row(depth, describe(s, nil)+"  "+f.took(s, s))
	}
}

// header is the command's row: its name and how long it has run, and,
// once it has been interrupted, how many targets will stop and how many
// will not start.
func (f *frame) header(s *treeSpan) string {
	g := f.t.g
	text := s.name
	select {
	case <-f.t.interrupted:
		var running, queued int
		for _, root := range f.t.tally.Roots() {
			running += root.Running
			queued += root.Queued
		}
		text += g.sep + "interrupting"
		if running > 0 {
			text += fmt.Sprintf("%s%d running will stop", g.sep, running)
		}
		if queued > 0 {
			text += fmt.Sprintf("%s%d queued will not start", g.sep, queued)
		}
	default:
	}
	return text + g.sep + elapsed(f.now.Sub(s.started))
}

// below draws what is below s at depth: the targets that ended otherwise
// than well, the spans under way, the targets running, and the targets
// that ended well.
func (f *frame) below(s *treeSpan, depth int) {
	g := f.t.g
	pad := indent(depth)
	folded := s.ended
	if folded != nil && len(folded.failures) > 0 {
		failures := &part{rank: 1, cut: func(left []int) string {
			return pad + g.failed + " " + g.more + " " + more(left, len(folded.failures), "failed")
		}}
		for _, fl := range folded.failures {
			failures.rows = append(failures.rows, fmt.Sprintf("%s%s %s  %s", pad, g.failed, fl.names.String(), fl.text))
			failures.weights = append(failures.weights, fl.names.len())
		}
		f.parts = append(f.parts, failures)
	}
	if folded != nil && folded.canceled.len() > 0 {
		f.row(depth, g.canceled+" "+folded.canceled.String()+"  canceled")
	}
	if folded != nil && folded.skipped.len() > 0 {
		text := g.skipped + " " + folded.skipped.String() + "  skipped"
		if folded.reason != "" {
			text += ": " + folded.reason
		}
		f.row(depth, text)
	}
	var running []*treeSpan
	for _, c := range s.children {
		if c.kind != progress.KindTarget {
			f.span(c, depth)
		} else if c.state == progress.StateRunning {
			running = append(running, c)
		}
	}
	if len(running) > 0 {
		// The longest running first, by the seconds a row shows: those
		// that started in the same second stay in the order they were
		// queued, rather than in the order a pool happened to start them.
		slices.SortStableFunc(running, func(a, b *treeSpan) int {
			return cmp.Compare(f.now.Sub(b.ran)/time.Second, f.now.Sub(a.ran)/time.Second)
		})
		list := &part{cut: func(left []int) string { return pad + g.more + " " + more(left, len(running), "running") }}
		for _, target := range running {
			list.rows = append(list.rows, pad+f.target(target))
		}
		f.parts = append(f.parts, list)
	}
	if folded != nil && folded.ok.len() > 0 {
		f.row(depth, g.ok+" "+folded.ok.String())
	}
}

// more says how many of a list's rows are left out, given what each stands
// for: "6 more running", or "6 running" when none of the list is shown.
func more(left []int, of int, what string) string {
	n := 0
	for _, w := range left {
		n += w
	}
	if len(left) < of {
		return fmt.Sprintf("%d more %s", n, what)
	}
	return fmt.Sprintf("%d %s", n, what)
}

// target is the row of a running target: its name, how long it has run,
// and what it waits for now, or the last line of its output.
func (f *frame) target(s *treeSpan) string {
	text := f.t.g.running + " " + s.name
	inner := f.innermost(s)
	text += "  " + f.took(s, inner)
	switch {
	case s.flags&progress.ShowLines != 0 && s.line != "":
		text += "  " + s.line
	case inner != nil:
		text += "  " + describe(inner, s)
	}
	return text
}

// innermost returns the innermost span under s that is running and drawn,
// the request a target waits for; nil for none.
func (f *frame) innermost(s *treeSpan) *treeSpan {
	var inner *treeSpan
	for cur := s; ; {
		var next *treeSpan
		for _, c := range slices.Backward(cur.children) {
			if c.state == progress.StateRunning && (c.flags&progress.Hidden == 0 || f.now.Sub(c.ran) >= revealAfter) {
				next = c
				break
			}
		}
		if next == nil {
			return inner
		}
		inner, cur = next, next
	}
}

// call returns the newest call made for s itself that is running and
// drawn; nil for none.
func (f *frame) call(s *treeSpan) *treeSpan {
	for _, c := range slices.Backward(s.children) {
		if c.kind == progress.KindCall && c.state == progress.StateRunning &&
			(c.flags&progress.Hidden == 0 || f.now.Sub(c.ran) >= revealAfter) {
			return c
		}
	}
	return nil
}

// took says how long s has run, or, when the request it waits for has a
// bound, how long that has run against it: "3m12s/10m".
func (f *frame) took(s, inner *treeSpan) string {
	if inner != nil && inner.fields.Timeout > 0 {
		return seconds(f.now.Sub(inner.ran)) + "/" + bound(inner.fields.Timeout)
	}
	return seconds(f.now.Sub(s.ran))
}

// describe says what a span is doing, in few words: the method and path of
// a Redfish request, or the name of a span with the node it is for, when
// that is not of, the target it is made for, and its message.
func describe(s, of *treeSpan) string {
	fl := s.fields
	if fl.Method != "" {
		return fl.Method + " " + fl.Path
	}
	text := s.name
	if s.kind == progress.KindCall && fl.Node != "" && (of == nil || fl.Node != of.name && fl.Node != of.fields.Node) {
		text += " " + fl.Node
	}
	if fl.Message != "" {
		text += " " + fl.Message
	}
	return text
}

// standingOf says how the targets of a step or a batch stand: how many are
// done, of how many, and how many of them failed, were interrupted or left
// out, run and wait.
func standingOf(c progress.Count, g glyphs) string {
	parts := []string{fmt.Sprintf("%d/%d", c.Done, c.Total)}
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
	return strings.Join(parts, g.sep)
}

// seconds reads d in whole seconds, so that a row changes once a second,
// not at every frame: "4s", "3m12s", "1h02m".
func seconds(d time.Duration) string {
	s := int(max(0, d) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s/60%60)
}

// bound reads the bound of a request as it would be written: "10m", "30s",
// "1m30s".
func bound(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return seconds(d)
	}
	return took(d)
}
