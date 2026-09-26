// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// lineLog keeps the lines OnLine is handed, which the two streams may hand
// it at the same time.
type lineLog struct {
	mu    sync.Mutex
	lines map[progress.Stream][]string
}

func (l *lineLog) add(st progress.Stream, line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lines == nil {
		l.lines = map[progress.Stream][]string{}
	}
	l.lines[st] = append(l.lines[st], line)
}

func (l *lineLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf("stdout %q, stderr %q", l.lines[progress.Stdout], l.lines[progress.Stderr])
}

// A parser is handed each line of both streams that ended, without its
// line ending, and never the part of one the output cut off; what the
// result keeps is the output as it came, bounded as before, though the
// parser sees past the bound.
func TestRunHandsAParserTheLinesThatEnded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, script string
		limit        int
		stdout       string
		lines        string
	}{
		{"both streams", `printf 'one\ntwo\r\nthree'; printf 'warn\n' >&2`, 0, "one\ntwo\r\nthree",
			`stdout ["one" "two"], stderr ["warn"]`},
		{"past the bound", `printf 'abcdef\nxyz\n'`, 4, "abcd",
			`stdout ["abcdef" "xyz"], stderr []`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got lineLog
			result, err := fakeClient(t, tc.script).Run(context.Background(), target, transport.Request{
				Argv: []string{"true"}, MaxOutput: tc.limit, OnLine: got.add,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Stdout != tc.stdout {
				t.Errorf("stdout = %q, want %q", result.Stdout, tc.stdout)
			}
			if got.String() != tc.lines {
				t.Errorf("OnLine was handed %s, want %s", &got, tc.lines)
			}
		})
	}
}

// The lines of a call whose step shows them reach a display as they come,
// escaped, and a last line the output ended without a newline reaches it
// before the call ends: Run returns only once the output has been read.
func TestRunShowsTheLastLineBeforeItsCallEnds(t *testing.T) {
	t.Parallel()
	c := &progresstest.Capture{Lines: true}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	ctx, step := progress.Start(progress.WithBus(context.Background(), bus), progress.KindStep, "exec",
		progress.WithFlags(progress.ShowLines))
	runner := transport.Traced(fakeClient(t, `printf 'one\n'; printf '\033]52;c;aGk=\007partial'`), false)
	result, err := runner.Run(ctx, target, transport.Request{Argv: []string{"true"}})
	if err != nil || result.Failed() {
		t.Fatalf("Run = %v, %v", result, err)
	}
	step.End(nil)
	bus.Close()
	events := c.Events()
	progresstest.Check(t, events)

	var shown []string
	var call progress.SpanID
	for _, e := range events {
		switch {
		case e.Type == progress.TypeStart && e.Kind == progress.KindCall:
			call = e.Span
		case e.Type == progress.TypeLine:
			if e.Span != call {
				t.Errorf("line %q is shown under span %s, not the call's", e.Text, e.Span)
			}
			shown = append(shown, e.Text)
		case e.Type == progress.TypeEnd && e.Span == call:
			shown = append(shown, "end")
		}
	}
	if want := []string{"one", `\x1b]52;c;aGk=\x07partial`, "end"}; !slices.Equal(shown, want) {
		t.Errorf("the display was shown %q, want %q", shown, want)
	}
}

// A recorder hands its answers on as Run hands on what ssh prints, so a
// test can drive a parser with prepared output, whichever way the answer
// was prepared.
func TestRecorderHandsItsAnswersOnLineByLine(t *testing.T) {
	t.Parallel()
	answer := &transport.Result{Stdout: "a\nb\npartial", Stderr: "e\n"}
	for _, tc := range []struct {
		name string
		rec  *transport.Recorder
	}{
		{"a reply", &transport.Recorder{Reply: func(transport.Target, transport.Request) (*transport.Result, error) {
			result := *answer
			return &result, nil
		}}},
		{"an answer by target", &transport.Recorder{ByTarget: map[string]*transport.Result{target.Name: answer}}},
		{"an answer in turn", &transport.Recorder{Responses: []*transport.Result{answer}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got lineLog
			result, err := tc.rec.Run(context.Background(), target, transport.Request{Argv: []string{"true"}, OnLine: got.add})
			if err != nil || result.Stdout != answer.Stdout {
				t.Fatalf("Run = %v, %v", result, err)
			}
			if want := `stdout ["a" "b"], stderr ["e"]`; got.String() != want {
				t.Errorf("OnLine was handed %s, want %s", &got, want)
			}
		})
	}
}
