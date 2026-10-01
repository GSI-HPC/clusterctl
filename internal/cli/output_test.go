// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// TestBadQueryIsRejectedBeforeTheCommandRuns covers review finding 2.12: the
// expression was compiled only when the result was printed, after the
// command had run on the nodes.
func TestBadQueryIsRejectedBeforeTheCommandRuns(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{"jq=.[", "jsonpath={.[*].target.name}"} {
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

// TestExecJQSelectsFailingTargets covers review finding 12.3 with the idiom
// the manual documents.
func TestExecJQSelectsFailingTargets(t *testing.T) {
	t.Parallel()
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if tg.Name == "exe0002" {
			return &transport.Result{Target: tg, ExitCode: 1}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "exec", "-n", "exe[1-2]",
		"-o", "jq=.[] | select(.exitCode != 0) | .target.name", "--", "true")
	wantCode(t, err, exitcode.TargetFailed)
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
	t.Parallel()
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

// TestNodeFormatsRefuseAnOutputThatListsNoNodes: -o nodeset and -o name read
// the first column of any table as host names, so node attrs vendor printed
// vendor[1-2] and slurm job list the ids of the jobs, with exit 0, and a
// command that printed no table printed nothing and succeeded.
func TestNodeFormatsRefuseAnOutputThatListsNoNodes(t *testing.T) {
	t.Parallel()
	jobs := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: slurm.Render(req,
			slurm.Row{"i": "4711", "u": "alice", "T": "RUNNING", "N": "exe0001"})}, nil
	}}
	tests := []struct {
		rec  *transport.Recorder
		args []string
	}{
		{nil, []string{"node", "attrs", "vendor"}},
		{jobs, []string{"slurm", "job", "list"}},
		{nil, []string{"config", "view"}},
	}
	for _, tt := range tests {
		for _, format := range []string{"nodeset", "name"} {
			t.Run(strings.Join(tt.args, " ")+" -o "+format, func(t *testing.T) {
				h, err := run(t, harnessOptions{recorder: tt.rec}, append(tt.args, "-o", format)...)
				wantCode(t, err, exitcode.Usage)
				if err == nil || !strings.Contains(err.Error(), "lists none") {
					t.Errorf("err = %v, want it to say the command lists no nodes", err)
				}
				if h.out.Len() != 0 {
					t.Errorf("printed %q, want nothing", h.out)
				}
			})
		}
	}

	h, err := run(t, harnessOptions{}, "exec", "-n", "exe[1-2]", "-o", "name", "--", "true")
	if err != nil {
		t.Fatalf("exec -o name failed: %v", err)
	}
	if got, want := h.out.String(), "exe0001\nexe0002\n"; got != want {
		t.Errorf("exec -o name = %q, want %q", got, want)
	}
}

// TestNodeFormatsAreRefusedBeforeAChange: config init -o name wrote the files
// and printed their paths as host names, and boot sync and bmc redfish post
// acted before printing nothing. A command that changes something and lists
// no nodes refuses -o nodeset and -o name before it acts, as it refuses a jq
// program that does not compile.
func TestNodeFormatsAreRefusedBeforeAChange(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"nodeset", "name"} {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("config init -o %s, dry run %t", format, dryRun), func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "new")
				args := []string{"config", "init", dir, "-o", format}
				if dryRun {
					args = append(args, "--dry-run")
				}
				h, err := run(t, harnessOptions{}, args...)
				wantCode(t, err, exitcode.Usage)
				if h.out.Len() != 0 {
					t.Errorf("printed %q, want nothing", h.out)
				}
				if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s was created (%v)", dir, err)
				}
			})
		}
		for _, args := range [][]string{
			{"boot", "sync", "-y"},
			{"bmc", "redfish", "post", "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
				`{"ResetType":"ForceRestart"}`, "-n", "exe1", "-y"},
		} {
			t.Run(strings.Join(args[:2], " ")+" -o "+format, func(t *testing.T) {
				h, err := run(t, harnessOptions{recorder: sinfoAnswers("exe0001 idle\n", 0)},
					append(args, "-o", format)...)
				wantCode(t, err, exitcode.Usage)
				if err == nil || !strings.Contains(err.Error(), "lists none") {
					t.Errorf("err = %v, want it to say the command lists no nodes", err)
				}
				wantNoCalls(t, h)
			})
		}
	}
}
