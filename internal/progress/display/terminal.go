// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package display shows the progress of a command on its standard error,
// from the events of its progress.Bus: the live tree, a few rows at the
// bottom of a terminal that show the work under way and what of it failed;
// the counter, one line there that says how far the work has got; plain
// lines, one for each thing worth a line, for a log as much as a terminal;
// and the summary a display leaves behind once the command has ended.
//
// The tree and the counter own the bottom rows of the terminal, the region,
// and nothing else. The command's own output goes to the terminal through
// the writers of the Terminal, which take the region off before anything
// else is written, and let it back only once what was written ended a line,
// so that a question waiting for its answer is never drawn over. The lines
// a display leaves for good, plain lines and the tree's finished steps, go
// out through the Terminal too: above the region, never into a line the
// command has not ended, and ahead of whatever the command writes after the
// events they tell of. A display leaves the terminal altogether while a
// question is asked, through progress.Suspend, and never hides the cursor,
// so a process killed while it draws leaves a terminal that works, at worst
// with the rows of its last frame on it.
package display

import (
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/GSI-HPC/clusterctl/internal/output"
)

const (
	// eraseLine takes the line the cursor is on off the terminal, and
	// puts the cursor at its start.
	eraseLine = "\r\x1b[2K"
	// eraseLineAbove moves the cursor up a line and takes that line off.
	// Every terminal, tmux and screen among them, knows both.
	eraseLineAbove = "\x1b[1A\x1b[2K"
)

// defaultWidth is the width assumed of a terminal that does not say.
const defaultWidth = 80

// Terminal is the terminal a display draws on, shared with the command's
// own output. It is safe for concurrent use.
type Terminal struct {
	w    io.Writer
	size func() (w, h int, err error)

	mu sync.Mutex
	// rows are what the display has on the terminal, the region, with
	// the cursor at the end of the last; none for nothing.
	rows []string
	// open says the last write left a line unfinished, a question waiting
	// for its answer, perhaps: the display stays off until a write ends
	// the line, or the question is answered.
	open bool
	// suspended counts the Suspends not yet resumed.
	suspended int
	closed    bool
	// held, when a display leaves lines for good, plain lines or the
	// tree's finished steps, returns the lines it has not written yet,
	// and forgets them.
	held func() string

	// PanicLog receives the stack of a display that panicked while it
	// drew, the front end's diagnostics; nil is the terminal itself.
	PanicLog io.Writer
	// Foreground, when it is set, reports whether the process is the job
	// in the terminal's foreground. A job in the background draws nothing:
	// the shell's prompt and what is typed at it are on the rows a frame
	// would erase.
	Foreground func() bool
}

// NewTerminal returns the terminal w is, whose width and height size tells;
// nil size is a terminal 80 columns wide.
func NewTerminal(w io.Writer, size func() (w, h int, err error)) *Terminal {
	return &Terminal{w: w, size: size}
}

// recovered, deferred by the goroutine a display draws from, keeps a panic
// there from ending the process, as the Bus keeps one in a sink from: the
// display draws no more, its region comes off the terminal once the stack
// is written to PanicLog, and the command goes on. Close still writes what
// the display left.
func (t *Terminal) recovered() {
	p := recover()
	if p == nil {
		return
	}
	log := t.PanicLog
	if log == nil {
		log = t.w
	}
	// The stack is a courtesy; one that cannot be written changes nothing.
	_, _ = fmt.Fprintf(t.Writer(log), "clusterctl: the progress display stopped: %v\n%s", p, debug.Stack())
}

// Writer returns a writer to w, a stream that shows on the terminal, such as
// standard error or standard output, that takes the display's region off
// before it writes. What is written is not changed.
func (t *Terminal) Writer(w io.Writer) io.Writer {
	return writer{t: t, w: w}
}

type writer struct {
	t *Terminal
	w io.Writer
}

func (w writer) Write(p []byte) (int, error) {
	t := w.t
	t.mu.Lock()
	defer t.mu.Unlock()
	t.erase()
	t.release(false)
	n, err := w.w.Write(p)
	if n > 0 {
		t.open = p[n-1] != '\n'
	}
	return n, err
}

// draw puts rows on the terminal in place of the display's region, each cut
// to the width, once the lines a display holds are written above it,
// unless the terminal is lent out, closed or waiting for a line to end. No
// rows take the region off.
func (t *Terminal) draw(rows []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.suspended > 0 || t.open {
		return
	}
	if t.Foreground != nil && !t.Foreground() {
		// The rows drawn before are the shell's now, to write over.
		t.rows = nil
		return
	}
	width, _ := t.dims()
	cutRows := make([]string, 0, len(rows))
	for _, row := range rows {
		cutRows = append(cutRows, cut(row, width-1))
	}
	var held string
	if t.held != nil {
		held = t.held()
	}
	if held == "" && slices.Equal(cutRows, t.rows) {
		return
	}
	if len(cutRows) == 0 && held == "" {
		t.erase()
		return
	}
	// One write for the whole frame, so that the terminal shows no frame
	// half drawn. The terminal is a courtesy; a frame that cannot be
	// drawn changes nothing about the command.
	var b strings.Builder
	b.WriteString(eraseLine)
	for range len(t.rows) - 1 {
		b.WriteString(eraseLineAbove)
	}
	b.WriteString(held)
	b.WriteString(strings.Join(cutRows, "\n"))
	_, _ = io.WriteString(t.w, b.String())
	t.rows = cutRows
}

// erase takes the display's region off the terminal, and leaves the cursor
// at the start of the row the region began on. t.mu is held.
func (t *Terminal) erase() {
	if len(t.rows) == 0 {
		return
	}
	_, _ = io.WriteString(t.w, eraseLine+strings.Repeat(eraseLineAbove, len(t.rows)-1))
	t.rows = nil
}

// release writes the lines a display holds, unless the terminal
// is lent out or waiting for a line to end. forced, as the display closes,
// it writes them all the same, after ending such a line. t.mu is held.
func (t *Terminal) release(forced bool) {
	if t.held == nil || t.closed || !forced && (t.suspended > 0 || t.open) {
		return
	}
	text := t.held()
	if text == "" {
		return
	}
	if t.open {
		text = "\n" + text
	}
	_, _ = io.WriteString(t.w, text)
	t.open = false
}

// flush writes the lines a display holds, when it may.
func (t *Terminal) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.release(false)
}

// dims returns how many columns and rows the terminal has, as it says now:
// a terminal that does not say is 80 columns wide and of a height not
// known, 0. It needs no lock.
func (t *Terminal) dims() (width, height int) {
	if t.size != nil {
		if w, h, err := t.size(); err == nil && w > 0 {
			return w, max(h, 0)
		}
	}
	return defaultWidth, 0
}

// suspend takes the display off the terminal until resume, for a question to
// be asked there, once the lines it holds are written.
func (t *Terminal) suspend() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.erase()
	t.release(false)
	t.suspended++
}

// resume lets the display back once every suspend has been resumed. The
// question has been answered by then, so the line it left open has ended.
func (t *Terminal) resume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.suspended > 0 {
		t.suspended--
	}
	t.open = false
	t.release(false)
}

// close takes the display off the terminal for good, once the lines it
// holds are written. The writers go on writing.
func (t *Terminal) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.erase()
	t.release(true)
	t.closed = true
}

// cut shortens s to at most n columns, so that a row never wraps: erasing
// it would take off only the last line of it. A wide character, as CJK
// text and emoji are, takes two, and one that would reach past n is left
// out whole.
func cut(s string, n int) string {
	if n < 1 {
		return ""
	}
	if output.Width(s) <= n {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		w := output.RuneWidth(r)
		if w > n {
			break
		}
		b.WriteRune(r)
		n -= w
	}
	return b.String()
}
