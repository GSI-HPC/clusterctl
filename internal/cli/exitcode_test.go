// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// exitCodeOf runs a command line the way the process does, through report,
// and returns the exit code it ends with. run returns the error of Execute
// and skips report, so it cannot see what a script would.
func exitCodeOf(t *testing.T, opts harnessOptions, args ...string) (*harness, int) {
	t.Helper()
	h, cmd := build(t, opts, args...)
	ctx := opts.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return h, execute(ctx, cmd, h.streams)
}

// TestUsageErrorsExitTwo checks that what cobra and pflag reject is a usage
// error. Each of these exited 1, which a wrapper reads as "some nodes are
// unhealthy", and a misspelt subcommand below the root printed its group's
// help and exited 0, so that "&& clusterctl bmc power off" went ahead.
func TestUsageErrorsExitTwo(t *testing.T) {
	tests := [][]string{
		{"bmc", "power", "off", "--bogus", "-n", "exe0001"},
		{"bmc", "web"},
		{"--fanout", "abc", "version"},
		{"bmcx"},
		{"bmc", "power", "off", "-n"},
		{"version", "extra"},
		{"-o", "bogus", "version"},
		{"slurm", "node", "drian", "ticket 42", "-n", "exe0007", "-y"},
		{"slurm", "node", "drian", "x", "-n", "exe0001", "-y"},
		{"-o", "json", "slurm", "node", "drian", "x", "-n", "exe0001", "-y"},
		{"bmc", "powr", "off", "-n", "exe0001", "-y"},
		{"provision", "reinstal", "-n", "exe0001", "-y"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, code := exitCodeOf(t, harnessOptions{}, args...)
			if code != exitcode.Usage {
				t.Errorf("exit code = %d, want %d; stderr:\n%s", code, exitcode.Usage, h.errOut)
			}
			if h.out.Len() != 0 {
				t.Errorf("a usage error printed to stdout, where a script reads results:\n%s", h.out)
			}
			if calls := h.recorder.Calls(); len(calls) != 0 {
				t.Errorf("a usage error sent %d requests", len(calls))
			}
		})
	}
}

// TestUnknownSubcommandIsNamed checks that the refusal says which word was
// not understood and suggests what was probably meant.
func TestUnknownSubcommandIsNamed(t *testing.T) {
	h, code := exitCodeOf(t, harnessOptions{}, "slurm", "node", "drian", "x", "-n", "exe0001", "-y")
	if code != exitcode.Usage {
		t.Fatalf("exit code = %d, want %d", code, exitcode.Usage)
	}
	for _, want := range []string{`unknown command "drian" for "clusterctl slurm node"`, "drain"} {
		if !strings.Contains(h.errOut.String(), want) {
			t.Errorf("stderr does not say %q:\n%s", want, h.errOut)
		}
	}
}

// TestGroupWithoutArgumentsPrintsHelp checks that a group on its own still
// lists its subcommands and succeeds.
func TestGroupWithoutArgumentsPrintsHelp(t *testing.T) {
	for _, args := range [][]string{{"slurm", "node"}, {}} {
		h, code := exitCodeOf(t, harnessOptions{}, args...)
		if code != exitcode.OK {
			t.Errorf("%q: exit code = %d, want 0; stderr:\n%s", args, code, h.errOut)
		}
		if !strings.Contains(h.out.String(), "Available Commands") {
			t.Errorf("%q: no help was printed:\n%s", args, h.out)
		}
	}
}

// TestFailedCommandOnARoleIsNotUnreachable checks that a tool failing on an
// infrastructure host is reported as a failure with what it said, not as a
// host that could not be reached. The host answered; exit 3 sent the
// administrator to check the network, and the tool's message was dropped.
func TestFailedCommandOnARoleIsNotUnreachable(t *testing.T) {
	const complaint = "ibwarn: mad_rpc_open_port: can't open UMAD port"
	failing := func() *transport.Recorder {
		return &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
			return transport.ExitResult(tg, 1, "", complaint+"\n"), nil
		}}
	}
	for _, args := range [][]string{
		{"fabric", "counters", "exe0001"},
		{"boot", "status", "-n", "exe0001"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, code := exitCodeOf(t, harnessOptions{recorder: failing()}, args...)
			if code != exitcode.TargetFailed {
				t.Errorf("exit code = %d, want %d; stderr:\n%s", code, exitcode.TargetFailed, h.errOut)
			}
			if !strings.Contains(h.errOut.String(), complaint) {
				t.Errorf("stderr does not carry what the host said:\n%s", h.errOut)
			}
		})
	}
}

// TestUnreachableRoleIsUnreachable checks the other side: ssh's own failure
// on a role still exits 3.
func TestUnreachableRoleIsUnreachable(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return transport.ExitResult(tg, 255, "", "ssh: connect to host ibgw01 port 22: Connection refused\n"), nil
	}}
	h, code := exitCodeOf(t, harnessOptions{recorder: rec}, "fabric", "counters", "exe0001")
	if code != exitcode.Transport {
		t.Errorf("exit code = %d, want %d; stderr:\n%s", code, exitcode.Transport, h.errOut)
	}
}

// TestInterruptedCommandExits130 checks that a command which fails after the
// process was interrupted exits 130, however the failure was worded. Paths
// that flatten an error to its text, or a request that failed because the
// interrupt stopped it, would otherwise report the administrator's own Ctrl-C
// as a failed or unreachable host.
func TestInterruptedCommandExits130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		// The interrupt arrives while the command runs.
		cancel()
		return transport.ExitResult(tg, 255, "", "Killed by signal 2.\n"), nil
	}}
	h, code := exitCodeOf(t, harnessOptions{recorder: rec, ctx: ctx},
		"slurm", "node", "drain", "ticket 1", "-n", "exe[0001-0003]", "-y")
	if code != exitcode.Interrupted {
		t.Errorf("exit code = %d, want %d; stderr:\n%s", code, exitcode.Interrupted, h.errOut)
	}
	if !strings.Contains(h.errOut.String(), "interrupted") {
		t.Errorf("stderr does not say the command was interrupted:\n%s", h.errOut)
	}
}

// TestInterruptEndsTheStdinRead checks that exec --stdin gives up waiting for
// its input when interrupted, and sends nothing. The read ignored the
// context, so Ctrl-C was lost while it waited.
func TestInterruptEndsTheStdinRead(t *testing.T) {
	stdin, typing := io.Pipe()
	t.Cleanup(func() { _ = typing.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	type outcome struct {
		h    *harness
		code int
	}
	done := make(chan outcome, 1)
	go func() {
		h, code := exitCodeOf(t, harnessOptions{in: stdin, ctx: ctx}, "exec", "--stdin", "-n", "exe[1-2]", "--", "cat")
		done <- outcome{h, code}
	}()
	select {
	case got := <-done:
		if got.code != exitcode.Interrupted {
			t.Errorf("exit code = %d, want %d; stderr:\n%s", got.code, exitcode.Interrupted, got.h.errOut)
		}
		if calls := got.h.recorder.Calls(); len(calls) != 0 {
			t.Errorf("%d requests were sent after the interrupt", len(calls))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec --stdin kept reading after the interrupt")
	}
}
