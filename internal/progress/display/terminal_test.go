// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display_test

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
)

// terminalFixture is a counter on a Screen, drawn from a clock two seconds
// on, so that its first Draw shows it.
type terminalFixture struct {
	screen  *progresstest.Screen
	term    *display.Terminal
	counter *display.Counter
	clock   *clock
}

func newTerminalFixture(t *testing.T) *terminalFixture {
	t.Helper()
	f := &terminalFixture{screen: &progresstest.Screen{}, clock: &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}}
	f.term = display.NewTerminal(f.screen, nil)
	f.counter = display.NewCounter(f.term, display.CounterOptions{Now: f.clock.Now})
	f.clock.Add(2 * time.Second)
	t.Cleanup(f.counter.Close)
	return f
}

// shows fails the test unless the Screen shows want, every row ended by a
// newline, the cursor's row last.
func (f *terminalFixture) shows(t *testing.T, what, want string) {
	t.Helper()
	if got := f.screen.String(); got != want {
		t.Errorf("%s, the screen shows:\n%q\nwant:\n%q", what, got, want)
	}
}

// A panic's stack from a pool's worker, written while a password is asked
// for, waits until the question has been answered, and is not written
// into it: neither into clusterctl's own prompt, which the command writes
// and ends through the Terminal while the display is suspended, nor into
// that of a helper, such as sops, which writes to the terminal itself.
func TestLinesWaitWhileAQuestionIsAsked(t *testing.T) {
	t.Parallel()
	f := newTerminalFixture(t)
	diag := f.term.Lines(f.screen)
	errOut := f.term.Writer(f.screen)
	f.counter.Draw()

	f.counter.Suspend()
	_, _ = io.WriteString(errOut, "Password for bmc@node1: ")
	_, _ = io.WriteString(diag, "clusterctl: panic while working on exe1: \"boom\"\n")
	_, _ = io.WriteString(diag, "goroutine 7 [running]:\n")
	f.shows(t, "while the question is asked", "Password for bmc@node1: \n")
	// The terminal echoes nothing, so the command ends the line itself,
	// still suspended.
	_, _ = io.WriteString(errOut, "\n")
	f.shows(t, "once the line has ended", "Password for bmc@node1: \n")
	f.counter.Resume()
	f.shows(t, "once it is answered", "Password for bmc@node1: \n"+
		"clusterctl: panic while working on exe1: \"boom\"\n"+
		"goroutine 7 [running]:\n")

	f.counter.Suspend()
	_, _ = io.WriteString(f.screen, "Enter passphrase: ")
	_, _ = io.WriteString(diag, "a log line\n")
	_, _ = io.WriteString(f.screen, "\n")
	f.counter.Resume()
	if got := f.screen.String(); !strings.HasSuffix(got, "\nEnter passphrase: \na log line\n") {
		t.Errorf("around a helper's question, the screen shows:\n%q", got)
	}
}

// A line written while the command has a line of its own open, a question
// it asks, waits for the write that ends that line.
func TestLinesWaitForAnOpenLineToEnd(t *testing.T) {
	t.Parallel()
	f := newTerminalFixture(t)
	out := f.term.Writer(f.screen)
	diag := f.term.Lines(f.screen)
	_, _ = io.WriteString(out, "Reset 2 hosts? [y/N] ")
	_, _ = io.WriteString(diag, "a log line\n")
	f.counter.Draw()
	f.shows(t, "while the line is open", "Reset 2 hosts? [y/N] \n")
	_, _ = io.WriteString(out, "y\n")
	f.shows(t, "once it has ended", "Reset 2 hosts? [y/N] y\na log line\n")
}

// While the display's region is drawn, a line waits for the next frame,
// which writes it above the region and draws the region again.
func TestLinesAreWrittenAboveTheNextFrame(t *testing.T) {
	t.Parallel()
	f := newTerminalFixture(t)
	diag := f.term.Lines(f.screen)
	f.counter.Draw()
	_, _ = io.WriteString(diag, "first\n")
	f.shows(t, "before the next frame", "0:02\n")
	f.counter.Draw()
	f.shows(t, "after it", "first\n0:02\n")

	// Without a region, a line is written at once.
	f.counter.Close()
	_, _ = io.WriteString(diag, "second\n")
	f.shows(t, "once the display is closed", "first\nsecond\n")
}

// A write of the command's own writes the lines that wait before its bytes,
// so that the two keep the order they were written in.
func TestLinesComeBeforeTheCommandsNextWrite(t *testing.T) {
	t.Parallel()
	f := newTerminalFixture(t)
	f.counter.Draw()
	_, _ = io.WriteString(f.term.Lines(f.screen), "a log line\n")
	_, _ = io.WriteString(f.term.Writer(f.screen), "output\n")
	f.shows(t, "after the command's write", "a log line\noutput\n")
}

// A line that has not ended when the display closes is ended then, and the
// part of a line written after its end waits for the rest.
func TestCloseEndsALineOfLinesThatWasNotEnded(t *testing.T) {
	t.Parallel()
	f := newTerminalFixture(t)
	diag := f.term.Lines(f.screen)
	_, _ = io.WriteString(diag, "one\ntw")
	f.shows(t, "with a line begun", "one\n")
	_, _ = io.WriteString(diag, "o\nthr")
	f.shows(t, "with the next begun", "one\ntwo\n")
	f.counter.Close()
	f.shows(t, "once closed", "one\ntwo\nthr\n")
}

// A line that would reach past the bound, 256 KiB, while the lines wait is
// left out, and a line written with the others, which names the program,
// says how many were; a line that does not end within the bound is cut.
func TestWhatLinesHoldIsBounded(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	f := newTerminalFixture(t)
	f.term.Program = "sind"
	diag := f.term.Lines(&out)
	f.counter.Suspend()
	line := strings.Repeat("x", 1023) + "\n"
	for range 300 {
		_, _ = io.WriteString(diag, line)
	}
	f.counter.Resume()
	got := out.String()
	if n := strings.Count(got, line); n != 256 {
		t.Errorf("%d lines were written, want the 256 that fit in 256 KiB", n)
	}
	if !strings.HasSuffix(got, "\nsind: 44 lines were left out while the terminal was busy\n") {
		t.Errorf("no line says how many were left out: %q", got[len(got)-80:])
	}

	out.Reset()
	_, _ = io.WriteString(diag, strings.Repeat("y", 300<<10))
	if got := out.String(); len(got) != 300<<10+1 || !strings.HasSuffix(got, "y\n") {
		t.Errorf("a line longer than the bound was written as %d bytes, want it cut and ended", len(got))
	}
}

// A display that panics while it draws writes its stack as a line from
// beside the command, which waits while a question is asked.
func TestThePanicOfADisplayWaitsForTheQuestion(t *testing.T) {
	t.Parallel()
	var log progresstest.Screen
	s := &progresstest.Screen{}
	var mu sync.Mutex
	asking := false
	term := display.NewTerminal(s, func() (int, int, error) {
		mu.Lock()
		defer mu.Unlock()
		if asking {
			panic("the size of the terminal")
		}
		return 80, 24, nil
	})
	term.PanicLog = &log
	c := &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	counter := display.NewCounter(term, display.CounterOptions{Now: c.Now})
	c.Add(2 * time.Second)
	counter.Suspend()
	mu.Lock()
	asking = true
	mu.Unlock()
	counter.Start()
	time.Sleep(50 * time.Millisecond)
	if got := log.String(); got != "" {
		t.Errorf("the panic was written while the question was asked: %q", got)
	}
	counter.Resume()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasPrefix(log.String(), "the progress display stopped: the size of the terminal\n") {
		if time.Now().After(deadline) {
			t.Fatalf("no word of the panic within 5s of the answer: %q", log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	counter.Close()
}

// Lines from goroutines beside the command, written while the command
// writes its own lines, asks questions and the region is drawn, all reach
// the terminal, each whole on a row of its own, in the order each
// goroutine wrote them, and none inside a question.
func TestLinesStayWholeUnderInterleaving(t *testing.T) {
	t.Parallel()
	const writers, each = 4, 200
	f := newTerminalFixture(t)
	f.counter.Start()

	var wg sync.WaitGroup
	for g := range writers {
		diag := f.term.Lines(f.screen)
		wg.Go(func() {
			for i := range each {
				// Half the lines come in two writes, as a logger that
				// writes a line in parts would.
				if i%2 == 0 {
					_, _ = fmt.Fprintf(diag, "L%d-%d\n", g, i)
				} else {
					_, _ = fmt.Fprintf(diag, "L%d-", g)
					_, _ = fmt.Fprintf(diag, "%d\n", i)
				}
			}
		})
	}
	out := f.term.Writer(f.screen)
	for i := range each {
		_, _ = fmt.Fprintf(out, "W%d\n", i)
		if i%20 == 0 {
			f.counter.Suspend()
			_, _ = io.WriteString(out, "Password: ")
			time.Sleep(time.Millisecond)
			// The terminal echoes the answer's newline.
			_, _ = io.WriteString(f.screen, "\n")
			f.counter.Resume()
		}
		if i%7 == 0 {
			_, _ = io.WriteString(out, "Continue? ")
			_, _ = io.WriteString(out, "y\n")
		}
	}
	wg.Wait()
	f.counter.Close()

	row := regexp.MustCompile(`^(L(\d)-(\d+)|W(\d+)|Password: |Continue\? y)$`)
	next := make([]int, writers)
	commands := 0
	for _, line := range strings.Split(strings.TrimSuffix(f.screen.String(), "\n"), "\n") {
		m := row.FindStringSubmatch(line)
		switch {
		case m == nil:
			t.Fatalf("a torn row: %q", line)
		case m[2] != "":
			g, i := int(m[2][0]-'0'), 0
			_, _ = fmt.Sscan(m[3], &i)
			if i != next[g] {
				t.Fatalf("L%d-%d came where L%d-%d should have", g, i, g, next[g])
			}
			next[g]++
		case m[4] != "":
			commands++
		}
	}
	for g, n := range next {
		if n != each {
			t.Errorf("writer %d: %d lines reached the terminal, want %d", g, n, each)
		}
	}
	if commands != each {
		t.Errorf("%d lines of the command's reached the terminal, want %d", commands, each)
	}
}
