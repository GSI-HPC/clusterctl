// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// TestBadQueryIsRejectedBeforeTheCommandRuns covers review finding 2.12: the
// expression was compiled only when the result was printed, after the
// command had run on the nodes.
func TestBadQueryIsRejectedBeforeTheCommandRuns(t *testing.T) {
	for _, spec := range []string{"jq=.[", "jsonpath={[']}", `jsonpath={range .[*]}{.target.name}{end}`} {
		rec := &transport.Recorder{}
		_, err := run(t, harnessOptions{recorder: rec}, "exec", "-n", "exe0001", "-o", spec, "--", "touch", "/tmp/flag")
		if err == nil {
			t.Errorf("-o %s was accepted", spec)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("-o %s: exit code = %d, want %d", spec, got, want)
		}
		if calls := rec.Commands(); len(calls) != 0 {
			t.Errorf("-o %s: the command was sent before the expression was checked: %q", spec, calls)
		}
	}
}

// TestMalformedJSONPathDoesNotPanic covers review finding 10.2 through the
// command that does not load the site.
func TestMalformedJSONPathDoesNotPanic(t *testing.T) {
	_, err := run(t, harnessOptions{}, "version", "-o", "jsonpath={[']}")
	if err == nil {
		t.Fatal("a malformed jsonpath was accepted")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}

// TestExecJQSelectsFailingTargets covers review finding 12.3 with the idiom
// the manual documents.
func TestExecJQSelectsFailingTargets(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if tg.Name == "exe0002" {
			return &transport.Result{Target: tg, ExitCode: 1}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "exec", "-n", "exe[1-2]",
		"-o", "jq=.[] | select(.exitCode != 0) | .target.name", "--", "true")
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("exit code = %d, want %d (%v)", got, want, err)
	}
	if got, want := h.out.String(), "exe0002\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	h, _ = run(t, harnessOptions{recorder: rec}, "exec", "-n", "exe[1-2]", "-o", "yaml", "--", "true")
	if out := h.out.String(); !strings.Contains(out, "exitCode: 0\n") || strings.Contains(out, "0.0") {
		t.Errorf("-o yaml does not print integers as integers:\n%s", out)
	}
}

// TestJQStopsWithTheCommand covers review finding 10.2 the way the MCP
// server met it: read_command runs version with the call's context, and a
// jq program that never ends kept running after the client gave up.
func TestJQStopsWithTheCommand(t *testing.T) {
	for _, args := range [][]string{
		{"version", "-o", "jq=last(repeat(1))"},
		{"config", "init", "--dry-run", "-o", "jq=last(repeat(1))", "--site", "lab"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		dir := t.TempDir()
		var out bytes.Buffer
		streams := app.Streams{
			In: strings.NewReader(""), Out: &out, Err: &out,
			StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"),
		}
		cmd := NewRootCommand(ctx, streams)
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--config", exampleDir}, args...))
		if args[0] == "config" {
			cmd.SetArgs(append([]string{"--config", exampleDir}, append(args, filepath.Join(dir, "new"))...))
		}
		done := make(chan error, 1)
		go func() { done <- cmd.ExecuteContext(ctx) }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%q returned %v, want the deadline", args, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%q kept running after its context ended", args)
		}
		cancel()
	}
}
