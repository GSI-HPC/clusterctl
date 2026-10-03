// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-clikit/progress/progresstest"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// A line of white space from the gateway panicked, and a missing fping or an
// unreachable gateway was reported as every processor down, exit 1.
func TestBMCPingReadsTheSweepCarefully(t *testing.T) {
	t.Parallel()
	reply := func(r *transport.Result) *transport.Recorder {
		return &transport.Recorder{Responses: []*transport.Result{r}}
	}
	h, err := run(t, harnessOptions{recorder: reply(&transport.Result{
		Stdout: "exe0001.mgmt.hpc.example.org\n   \n", ExitCode: 1,
	})}, "bmc", "ping", "-n", "exe[0001-0002]")
	if got := exitcode.From(err); got != exitcode.TargetFailed {
		t.Errorf("one processor down: exit code %d, want %d (%v)", got, exitcode.TargetFailed, err)
	}
	// The processors travel on standard input, a line each, where none is
	// read as an option, and none is in the command.
	call := h.recorder.Calls()[0]
	if strings.Contains(call.Command, "exe0001") {
		t.Errorf("the command names a processor: %s", call.Command)
	}
	if got, want := pingedNames(call.Request), "exe0001.mgmt.hpc.example.org\nexe0002.mgmt.hpc.example.org\n"; got != want {
		t.Errorf("fping read %q, want %q", got, want)
	}

	for _, r := range []*transport.Result{
		{Stderr: "bash: fping: command not found\n", ExitCode: 127},
		{Stderr: "ssh: connect to host mgmt-gw.example.org port 22: No route to host\n", ExitCode: 255,
			Err: exitcode.Errorf(exitcode.Transport, "exit 255")},
	} {
		_, err := run(t, harnessOptions{recorder: reply(r)}, "bmc", "ping", "-n", "exe[0001-0002]")
		if got := exitcode.From(err); got != exitcode.Transport {
			t.Errorf("exit %d: exit code %d, want %d (%v)", r.ExitCode, got, exitcode.Transport, err)
		}
	}
}

// A dry run sent no sweep, read every processor as silent and exited 1,
// although a lookup that only reads runs for real in a dry run.
func TestBMCPingSweepsInADryRun(t *testing.T) {
	t.Parallel()
	rec := &transport.Recorder{Responses: []*transport.Result{{
		Stdout: "exe0001.mgmt.hpc.example.org\nexe0002.mgmt.hpc.example.org\n",
	}}}
	h, err := run(t, harnessOptions{recorder: rec}, "--dry-run", "bmc", "ping", "-n", "exe[0001-0002]")
	if err != nil {
		t.Fatalf("the dry run of a sweep that every processor answered failed: %v\n%s", err, h.out)
	}
	if commands := rec.Commands(); len(commands) != 1 || !strings.Contains(commands[0], "fping") {
		t.Errorf("the dry run sent %q, want the one sweep", commands)
	}
	if !strings.Contains(h.out.String(), "2 of 2 answered") {
		t.Errorf("the dry run does not report the sweep:\n%s", h.out)
	}
}

// The sweep is one step that knows how many processors it asks, with the
// one call that asks them all; fping says which answered only once it is
// done, so there is no target for each.
func TestBMCPingReportsTheSweepAsAStep(t *testing.T) {
	t.Parallel()
	ctx, watcher := progresstest.Watch(context.Background(), t, byExitCode)
	rec := &transport.Recorder{Responses: []*transport.Result{{Stdout: "exe0001.mgmt.hpc.example.org\n", ExitCode: 1}}}
	_, err := run(t, harnessOptions{ctx: ctx, recorder: rec}, "bmc", "ping", "-n", "exe[0001-0002]")
	wantCode(t, err, exitcode.TargetFailed)
	want := `command bmc ping: failed (target): 1 service processors did not answer
  step ping total=2: failed (target): 1 service processors did not answer
    call ssh node=mgmt host=mgmt-gw.example.org role=mgmt timeout=2m0s exit=1: failed (target): mgmt (mgmt-gw.example.org): command exited 1
`
	if got := watcher.Finish(); got != want {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want)
	}
}

// pingedNames is what a sweep handed fping on standard input: a recorded
// call's has been read, and is read again from its start.
func pingedNames(req transport.Request) string {
	stdin, ok := req.Stdin.(io.ReadSeeker)
	if !ok {
		return ""
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	data, _ := io.ReadAll(stdin)
	return string(data)
}

// The processors were named in fping's argument vector, which ssh sends as
// one argument of at most 128 KiB, so a sweep of some 4,000 was refused.
// They travel on standard input, so the command is the same for any number.
func TestBMCPingSweepsAnyNumberOfProcessors(t *testing.T) {
	t.Parallel()
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: big[0001-5000]\n"
	})
	rec := &transport.Recorder{Responses: []*transport.Result{{Stdout: "big0001.mgmt.example.org\n", ExitCode: 1}}}
	h, err := run(t, harnessOptions{config: []string{inventory}, recorder: rec}, "bmc", "ping", "-n", "big[0001-5000]")
	wantCode(t, err, exitcode.TargetFailed)
	if !strings.Contains(h.out.String(), "1 of 5000 answered") {
		t.Errorf("the sweep is not reported:\n%s", h.out)
	}
	call := rec.Calls()[0]
	if len(call.Command) > 256 {
		t.Errorf("the command is %d bytes: %s", len(call.Command), call.Command)
	}
	if n := strings.Count(pingedNames(call.Request), "\n"); n != 5000 {
		t.Errorf("fping read %d names, want 5000", n)
	}
}
