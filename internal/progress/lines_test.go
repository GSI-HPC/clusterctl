// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
)

// parser collects what Tee hands a parser.
type parser struct {
	mu    sync.Mutex
	lines []string
}

func (p *parser) parse(st progress.Stream, line string) {
	p.mu.Lock()
	p.lines = append(p.lines, st.String()+" "+line)
	p.mu.Unlock()
}

// shown returns the text of the lines the capture was sent.
func shown(c *progresstest.Capture) []string {
	var out []string
	for _, e := range c.Events() {
		if e.Type == progress.TypeLine {
			out = append(out, e.Text)
		}
	}
	return out
}

// showing returns a context with a running call that shows lines, and a
// capture that wants them.
func showing(t *testing.T, o progress.Options) (context.Context, *progress.Span, *progresstest.Capture) {
	t.Helper()
	capture := &progresstest.Capture{Lines: true}
	o.Sinks = []progress.Sink{capture}
	bus := progress.NewBus(o)
	t.Cleanup(func() {
		bus.Close()
		progresstest.Check(t, capture.Events())
	})
	ctx, _ := progress.Start(progress.WithBus(context.Background(), bus), progress.KindStep, "uptime", progress.WithFlags(progress.ShowLines))
	ctx, call := progress.Start(ctx, progress.KindCall, "ssh")
	return ctx, call, capture
}

func TestTeeIsTheWriterItselfWhenNothingWantsTheLines(t *testing.T) {
	t.Parallel()

	noLines := progress.NewBus(progress.Options{Sinks: []progress.Sink{&progresstest.Capture{}}})
	wantsLines := progress.NewBus(progress.Options{Sinks: []progress.Sink{&progresstest.Capture{Lines: true}}})
	defer noLines.Close()
	defer wantsLines.Close()
	shows := func(b *progress.Bus, f progress.Flags) context.Context {
		ctx, _ := progress.Start(progress.WithBus(context.Background(), b), progress.KindCall, "ssh", progress.WithFlags(f))
		return ctx
	}
	ended, span := progress.Start(shows(wantsLines, progress.ShowLines), progress.KindCall, "ssh")
	span.End(nil)

	tests := []struct {
		name string
		ctx  context.Context
	}{
		{"no Bus", context.Background()},
		{"no span", progress.WithBus(context.Background(), wantsLines)},
		{"a span without ShowLines", shows(wantsLines, 0)},
		{"no sink that wants lines", shows(noLines, progress.ShowLines)},
		{"a span that has ended", ended},
	}
	for _, tc := range tests {
		var buf bytes.Buffer
		if w := progress.Tee(tc.ctx, &buf, progress.Stdout, nil); w != &buf {
			t.Errorf("%s: Tee = %T, want the writer itself", tc.name, w)
		}
	}
}

func TestTeeHandsAParserCompleteLinesOnly(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 64<<10+1)
	tests := []struct {
		name   string
		writes []string
		want   []string
	}{
		{"one line", []string{"exe1: on\n"}, []string{"stdout exe1: on"}},
		{"a line in pieces", []string{"exe", "1: o", "n\nexe2: off\n"}, []string{"stdout exe1: on", "stdout exe2: off"}},
		{"a line ending in CRLF", []string{"exe1: on\r\n"}, []string{"stdout exe1: on"}},
		{"an empty line", []string{"\n"}, []string{"stdout "}},
		{"no line without its end", []string{"exe1: on\nexe2: "}, []string{"stdout exe1: on"}},
		{"no line longer than 64 KiB", []string{long[:40000], long[40000:] + "\nexe2: off\n"}, []string{"stdout exe2: off"}},
		{"the longest line there may be", []string{long[1:] + "\n"}, []string{"stdout " + long[1:]}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// With and without a display: the parser's lines are the same.
			for _, display := range []bool{false, true} {
				ctx, call := context.Background(), (*progress.Span)(nil)
				if display {
					ctx, call, _ = showing(t, progress.Options{})
				}
				var p parser
				var buf bytes.Buffer
				w := progress.Tee(ctx, &buf, progress.Stdout, p.parse)
				for _, s := range tc.writes {
					if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
						t.Fatalf("Write = %d, %v", n, err)
					}
				}
				call.End(nil)
				if got, want := strings.Join(p.lines, "|"), strings.Join(tc.want, "|"); got != want {
					t.Errorf("display %v: parsed %.100q, want %.100q", display, got, want)
				}
				if buf.String() != strings.Join(tc.writes, "") {
					t.Errorf("display %v: the writer got %.100q", display, buf.String())
				}
			}
		})
	}
}

func TestTeeShowsSanitisedLines(t *testing.T) {
	t.Parallel()

	ctx, call, capture := showing(t, progress.Options{})
	var stdout, stderr bytes.Buffer
	out := progress.Tee(ctx, &stdout, progress.Stdout, nil)
	errw := progress.Tee(ctx, &stderr, progress.Stderr, nil)
	fmt.Fprint(out, "up 3 days\n\x1b]52;c;ZXZpbA==\x07\n")
	fmt.Fprint(errw, "50%\r100%\r\n\n")
	call.End(nil)

	var got []string
	for _, e := range capture.Events() {
		if e.Type == progress.TypeLine {
			got = append(got, e.Stream.String()+" "+e.Text)
			if e.Span != capture.Events()[1].Span {
				t.Errorf("a line of span %s, want the call's", e.Span)
			}
		}
	}
	want := []string{"stdout up 3 days", `stdout \x1b]52;c;ZXZpbA==\x07`, "stderr 100%"}
	if !equal(got, want) {
		t.Errorf("lines %q, want %q", got, want)
	}
	// The command's own output is exactly what it wrote.
	if stdout.String() != "up 3 days\n\x1b]52;c;ZXZpbA==\x07\n" || stderr.String() != "50%\r100%\r\n\n" {
		t.Errorf("the writers got %q and %q", stdout.String(), stderr.String())
	}
}

func TestADisplayGetsALongLineInPiecesAndTheRestAtTheEnd(t *testing.T) {
	t.Parallel()

	ctx, call, capture := showing(t, progress.Options{})
	var p parser
	w := progress.Tee(ctx, io.Discard, progress.Stdout, p.parse)
	// After the "a", every two-byte rune starts at an odd offset, so the
	// 4 KiB mark falls inside one.
	long := "a" + strings.Repeat("é", 5000)
	fmt.Fprint(w, long[:3000])
	fmt.Fprint(w, long[3000:]+"\nunfinished")
	call.End(nil)

	// A piece, the rest of the line after the last piece, and the
	// unfinished line at the End.
	got := shown(capture)
	if len(got) != 4 {
		t.Fatalf("%d lines shown, want 4", len(got))
	}
	for i, piece := range got[:3] {
		// A piece that started inside a rune would start with an escaped
		// continuation byte.
		if want := "é"; i > 0 && !strings.HasPrefix(piece, want) {
			t.Errorf("piece %d starts %.12q, which splits a rune", i+1, piece)
		}
	}
	if got[3] != "unfinished" {
		t.Errorf("the last line shown is %q, want the unfinished one", got[3])
	}
	if len(p.lines) != 1 || p.lines[0] != "stdout "+long {
		t.Errorf("the parser got %d lines, want the whole of the long one only", len(p.lines))
	}
}

func TestLinesAreRateLimitedKeepingTheNewest(t *testing.T) {
	t.Parallel()

	clock := newClock()
	ctx, call, capture := showing(t, progress.Options{Now: clock.Now})
	w := progress.Tee(ctx, io.Discard, progress.Stdout, nil)
	for i := range 30 {
		fmt.Fprintf(w, "line %d\n", i)
	}
	if got := shown(capture); len(got) != 20 || got[19] != "line 19" {
		t.Fatalf("a burst showed %d lines, want the first 20", len(got))
	}

	// A token comes free every 100ms; the line held back is the newest.
	clock.Add(100 * time.Millisecond)
	fmt.Fprint(w, "partial")
	lines := capture.Events()
	last := lines[len(lines)-1]
	if last.Text != "line 29" || last.Dropped != 9 {
		t.Errorf("after 100ms the display got %q, %d dropped; want line 29, 9 dropped", last.Text, last.Dropped)
	}

	// With no token free, what is held back is sent at the End, and the
	// End counts everything left out.
	fmt.Fprint(w, "\nline 30\nline 31\n")
	call.End(nil)
	events := capture.Events()
	if got := shown(capture); got[len(got)-1] != "line 31" {
		t.Errorf("the last line shown is %q, want line 31", got[len(got)-1])
	}
	end := events[len(events)-1]
	if end.Type != progress.TypeEnd || end.Dropped != 11 {
		t.Errorf("the End reports %d lines dropped, want 11", end.Dropped)
	}
}

// The line held back after a burst is sent once its place comes, though
// nothing more is written: a node that prints a burst and then works on in
// silence shows the last line it printed, not one before it.
func TestTheLineHeldBackIsSentWhenItsPlaceComes(t *testing.T) {
	t.Parallel()

	ctx, call, capture := showing(t, progress.Options{})
	w := progress.Tee(ctx, io.Discard, progress.Stdout, nil)
	for i := range 21 {
		fmt.Fprintf(w, "line %d\n", i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := shown(capture)
		if len(got) > 0 && got[len(got)-1] == "line 20" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the line held back was not sent within 5s; shown: %q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	call.End(nil)
	if n := len(shown(capture)); n != 21 {
		t.Errorf("%d lines shown, want all 21 once", n)
	}
}

func TestNoLineIsShownOnceTheSpanHasEnded(t *testing.T) {
	t.Parallel()

	ctx, call, capture := showing(t, progress.Options{})
	var p parser
	w := progress.Tee(ctx, io.Discard, progress.Stdout, p.parse)
	call.End(nil)
	fmt.Fprint(w, "late\n")
	if got := shown(capture); len(got) != 0 {
		t.Errorf("lines shown after the End: %q", got)
	}
	// The parser still gets its lines: it is the command's, not the
	// display's.
	if len(p.lines) != 1 {
		t.Errorf("the parser got %q", p.lines)
	}
}

// failing is a writer that takes only part of what it is given.
type failing struct{}

func (failing) Write(p []byte) (int, error) { return len(p) / 2, errors.New("disk full") }

func TestTeeReportsWhatTheWriterDid(t *testing.T) {
	t.Parallel()

	ctx, call, capture := showing(t, progress.Options{})
	var p parser
	w := progress.Tee(ctx, failing{}, progress.Stdout, p.parse)
	n, err := w.Write([]byte("ab\ncd\n"))
	call.End(nil)
	if n != 3 || err == nil {
		t.Errorf("Write = %d, %v; want what the writer returned", n, err)
	}
	// The lines are what the writer took.
	if len(p.lines) != 1 || p.lines[0] != "stdout ab" || len(shown(capture)) != 1 {
		t.Errorf("parsed %q, shown %q", p.lines, shown(capture))
	}
}

// TestTeeKeepsUpWithAnEndWhileWriting is for the race detector: the two
// streams write, and a parser ends spans, while the call is ended.
func TestTeeKeepsUpWithAnEndWhileWriting(t *testing.T) {
	t.Parallel()

	ctx, call, _ := showing(t, progress.Options{})
	var p parser
	var wg sync.WaitGroup
	for _, st := range []progress.Stream{progress.Stdout, progress.Stderr} {
		w := progress.Tee(ctx, io.Discard, st, p.parse)
		wg.Go(func() {
			for i := range 200 {
				fmt.Fprintf(w, "line %d\npart", i)
			}
		})
	}
	call.End(nil)
	wg.Wait()
	if len(p.lines) != 400 {
		t.Errorf("the parser got %d lines, want 400", len(p.lines))
	}
}
