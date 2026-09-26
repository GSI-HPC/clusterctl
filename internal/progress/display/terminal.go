// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package display draws the progress of a command on the terminal of its
// standard error, from the events of its progress.Bus: for now the counter,
// one line that says how far the work has got.
//
// A display owns the bottom line of the terminal and nothing else. The
// command's own output goes to the terminal through the writers of the
// Terminal, which take the display's line off before anything else is
// written, and let it back only once what was written ended a line, so that
// a question waiting for its answer is never drawn over. The display leaves
// the terminal altogether while a question is asked, through
// progress.Suspend, and never hides the cursor, so a process killed while it
// draws leaves a terminal that works.
package display

import (
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/GSI-HPC/clusterctl/internal/output"
)

// eraseLine takes the line the cursor is on off the terminal, and puts the
// cursor at its start.
const eraseLine = "\r\x1b[2K"

// defaultWidth is the width assumed of a terminal that does not say.
const defaultWidth = 80

// Terminal is the terminal a display draws on, shared with the command's
// own output. It is safe for concurrent use.
type Terminal struct {
	w    io.Writer
	size func() (w, h int, err error)

	mu sync.Mutex
	// line is what the display has on the terminal, "" for nothing.
	line string
	// open says the last write left a line unfinished, a question waiting
	// for its answer, perhaps: the display stays off until a write ends
	// the line, or the question is answered.
	open bool
	// suspended counts the Suspends not yet resumed.
	suspended int
	closed    bool

	// PanicLog receives the stack of a display that panicked while it
	// drew, the front end's diagnostics; nil is the terminal itself.
	PanicLog io.Writer
	// Foreground, when it is set, reports whether the process is the job
	// in the terminal's foreground. A job in the background draws nothing:
	// the shell's prompt and what is typed at it are on the line the
	// display would erase.
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
// standard error or standard output, that takes the display's line off
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
	n, err := w.w.Write(p)
	if n > 0 {
		t.open = p[n-1] != '\n'
	}
	return n, err
}

// draw puts line on the terminal in place of the display's line, cut to the
// width, unless the terminal is lent out, closed or waiting for a line to
// end.
func (t *Terminal) draw(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.suspended > 0 || t.open {
		return
	}
	if t.Foreground != nil && !t.Foreground() {
		// The line drawn before is the shell's now, to write over.
		t.line = ""
		return
	}
	line = cut(line, t.width()-1)
	if line == t.line {
		return
	}
	if line == "" {
		t.erase()
		return
	}
	// The terminal is a courtesy; a line that cannot be drawn changes
	// nothing about the command.
	_, _ = io.WriteString(t.w, eraseLine+line)
	t.line = line
}

// erase takes the display's line off the terminal. t.mu is held.
func (t *Terminal) erase() {
	if t.line == "" {
		return
	}
	_, _ = io.WriteString(t.w, eraseLine)
	t.line = ""
}

// width returns how many columns the terminal has. t.mu is held.
func (t *Terminal) width() int {
	if t.size != nil {
		if w, _, err := t.size(); err == nil && w > 0 {
			return w
		}
	}
	return defaultWidth
}

// suspend takes the display off the terminal until resume, for a question to
// be asked there.
func (t *Terminal) suspend() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.erase()
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
}

// close takes the display off the terminal for good. The writers go on
// writing.
func (t *Terminal) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.erase()
	t.closed = true
}

// cut shortens s to at most n columns, so that a line never wraps: erasing
// it would take off only its last row. A wide character, as CJK text and
// emoji are, takes two, and one that would reach past n is left out whole.
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
