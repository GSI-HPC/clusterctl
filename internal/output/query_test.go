// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/output"
)

// TestParseFormatCompilesQueries covers review finding 2.12: an expression
// that cannot run is rejected when -o is read, before a command has acted,
// and not when the result is printed.
func TestParseFormatCompilesQueries(t *testing.T) {
	t.Parallel()

	for _, spec := range []string{
		"jq=.[",
		"jq=.[ |",
		"jq=$undefined",
		"jq=nosuchfunction(1)",
		"jsonpath={[']}",
		`jsonpath={["]}`,
		"jsonpath={.a",
		"jsonpath={.a[}",
		"jsonpath={.a[x]}",
		"jsonpath={..}",
		"jsonpath={..name}",
		"jsonpath={.a[?(@.b)]}",
		"jsonpath={.a[0,1]}",
		"jsonpath={.a b}",
		"jsonpath={.a.length()}",
	} {
		if _, err := output.ParseFormat(spec); err == nil {
			t.Errorf("ParseFormat(%q) should fail", spec)
		}
	}
}

// TestJSONPathRejectsUnsupportedSyntax covers review finding 12.8: the
// kubectl idiom with range printed an empty line and exited 0.
func TestJSONPathRejectsUnsupportedSyntax(t *testing.T) {
	t.Parallel()

	for _, spec := range []string{
		`jsonpath={range .[*]}{.name}{"\n"}{end}`,
		"jsonpath={range .[*]}",
		"jsonpath={end}",
		`jsonpath={"\n"}`,
		"jsonpath={'x'}",
	} {
		_, err := output.ParseFormat(spec)
		if err == nil {
			t.Errorf("ParseFormat(%q) should fail", spec)
			continue
		}
		if !strings.Contains(err.Error(), "jq") {
			t.Errorf("ParseFormat(%q) = %v, want it to point at -o jq", spec, err)
		}
	}
}

// TestJSONPathLoneQuoteDoesNotPanic covers review finding 10.2: a lone
// quote in brackets sliced inner[1:0] and crashed the process.
func TestJSONPathLoneQuoteDoesNotPanic(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"{[']}", `{["]}`, "{.a[']}", "{[ ' ]}"} {
		f := output.Format{Kind: output.FormatJSONPath, Arg: arg}
		if err := f.Write(&bytes.Buffer{}, output.Result{Object: []any{1}}); err == nil {
			t.Errorf("jsonpath %s should fail", arg)
		}
	}
}

// TestJQStopsWhenCancelled covers review finding 10.2: a program that never
// ends kept running after the caller gave up.
func TestJQStopsWhenCancelled(t *testing.T) {
	t.Parallel()

	for _, program := range []string{`repeat("xxxx")`, "def f: f; f", "last(range(1e18))"} {
		f, err := output.ParseFormat("jq=" + program)
		if err != nil {
			t.Fatalf("ParseFormat(%q): %v", program, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		done := make(chan error, 1)
		go func() { done <- f.WriteContext(ctx, discard{}, output.Result{Object: 1}) }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("jq=%s returned %v, want the deadline", program, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("jq=%s kept running after its context ended", program)
		}
		cancel()
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// TestQueriesKeepIntegers covers review finding 12.3: jq and jsonpath saw
// every number as a float64 and lost precision above 2^53.
func TestQueriesKeepIntegers(t *testing.T) {
	t.Parallel()

	r := output.Result{Object: map[string]any{"pid": 1234567, "big": uint64(9007199254740993)}}
	tests := map[string]string{
		"jq=.big":                "9007199254740993\n",
		"jq=.pid":                "1234567\n",
		"jq=.":                   "{\"big\":9007199254740993,\"pid\":1234567}\n",
		"jq=.big + 0":            "9007199254740993\n",
		"jsonpath={.big}":        "9007199254740993\n",
		"jsonpath={.pid}":        "1234567\n",
		"jsonpath={.big} {.pid}": "9007199254740993 1234567\n",
	}
	for spec, want := range tests {
		if got := render(t, spec, r); got != want {
			t.Errorf("-o %s = %q, want %q", spec, got, want)
		}
	}
}
