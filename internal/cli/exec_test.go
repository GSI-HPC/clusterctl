// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// wantNoCalls fails a test in which something was sent although the command
// should have stopped before contacting any host.
func wantNoCalls(t *testing.T, h *harness) {
	t.Helper()
	if calls := h.recorder.Commands(); len(calls) != 0 {
		t.Errorf("%d commands were sent, want none: %q", len(calls), calls)
	}
}

func wantCode(t *testing.T, err error, want int) {
	t.Helper()
	if got := exitcode.From(err); got != want {
		t.Errorf("exit code = %d (%v), want %d", got, err, want)
	}
}

func TestExecChecksProtectedHostsWithoutConfirm(t *testing.T) {
	// Not asking is the default for exec, but the protected hosts are not
	// optional: a plain run and a dry run have to make the same decision.
	h, err := run(t, harnessOptions{}, "exec", "-n", "exe0001,wlm01", "--", "reboot")
	wantCode(t, err, exitcode.Usage)
	if err == nil || !strings.Contains(err.Error(), "wlm01") {
		t.Errorf("error = %v, want it to name the protected host", err)
	}
	wantNoCalls(t, h)

	h, err = run(t, harnessOptions{}, "exec", "--force", "-n", "exe0001,wlm01", "--", "reboot")
	if err != nil {
		t.Fatalf("exec --force failed: %v", err)
	}
	if got, want := len(h.recorder.Calls()), 2; got != want {
		t.Errorf("got %d calls with --force, want %d", got, want)
	}
}

func TestExecStdinGoesThroughTheGate(t *testing.T) {
	script := []string{"--script", "cat > /etc/motd"}

	t.Run("a protected host is refused", func(t *testing.T) {
		h, err := run(t, harnessOptions{stdin: "hello\n"},
			append([]string{"exec", "--confirm", "--stdin", "-n", "exe0001,wlm01"}, script...)...)
		wantCode(t, err, exitcode.Usage)
		wantNoCalls(t, h)
	})

	t.Run("without a terminal it is refused", func(t *testing.T) {
		h, err := run(t, harnessOptions{stdin: "hello\n"},
			append([]string{"exec", "--confirm", "--stdin", "-n", "exe0001"}, script...)...)
		wantCode(t, err, exitcode.Usage)
		wantNoCalls(t, h)
	})

	t.Run("the answer is not read from the payload", func(t *testing.T) {
		// Standard input carries the payload, so a "y" in it must not
		// count as the answer.
		h, err := run(t, harnessOptions{stdin: "y\n", tty: true},
			append([]string{"exec", "--confirm", "--stdin", "-n", "exe0001"}, script...)...)
		wantCode(t, err, exitcode.Usage)
		if err == nil || !strings.Contains(err.Error(), "-y") {
			t.Errorf("error = %v, want it to point at -y", err)
		}
		wantNoCalls(t, h)
	})

	t.Run("-y confirms in advance", func(t *testing.T) {
		h, err := run(t, harnessOptions{stdin: "hello\n"},
			append([]string{"exec", "--confirm", "--stdin", "-y", "-n", "exe0001"}, script...)...)
		if err != nil {
			t.Fatalf("exec failed: %v", err)
		}
		if got, want := len(h.recorder.Calls()), 1; got != want {
			t.Errorf("got %d calls, want %d", got, want)
		}
	})

	t.Run("a dry run previews it", func(t *testing.T) {
		h, err := run(t, harnessOptions{stdin: "hello\n"},
			append([]string{"--dry-run", "exec", "--stdin", "-n", "exe0001"}, script...)...)
		if err != nil && !safety.IsDryRun(err) {
			t.Fatalf("dry run failed: %v", err)
		}
		if !strings.Contains(h.errOut.String(), "Would run a command on 1 host: exe0001") {
			t.Errorf("the dry run printed no preview:\n%s", h.errOut)
		}
		if !strings.Contains(h.errOut.String(), "6 bytes") {
			t.Errorf("the preview does not say what goes to standard input:\n%s", h.errOut)
		}
		wantNoCalls(t, h)
	})
}

func TestExecNeedsDashBeforeTheCommand(t *testing.T) {
	// Without --, the options of the remote command were read as
	// clusterctl's own: -r became --root and -n replaced the node set.
	for _, args := range [][]string{
		{"exec", "-n", "exe0001", "shutdown", "-r", "now"},
		{"exec", "-n", "exe0001", "grep", "-n", "wlm01", "/etc/hosts"},
		{"exec", "-n", "exe[1-4]", "tail", "-n", "100", "/var/log/messages"},
		{"exec", "-n", "exe0001", "uptime"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, args...)
			wantCode(t, err, exitcode.Usage)
			if err == nil || !strings.Contains(err.Error(), "--") {
				t.Errorf("error = %v, want it to say the command goes after --", err)
			}
			wantNoCalls(t, h)
		})
	}

	h, err := run(t, harnessOptions{}, "exec", "-n", "exe0001", "--", "shutdown", "-r", "now")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	calls := h.recorder.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if !strings.Contains(calls[0].Command, "shutdown -r now") {
		t.Errorf("command = %q, want the -r to reach the node", calls[0].Command)
	}
	if calls[0].Target.User == "root" {
		t.Error("the remote -r was taken as --root")
	}
}

func TestExecTakesTheNodeSetBeforeTheDash(t *testing.T) {
	t.Setenv(config.EnvNodes, "exe[0001-0010]")

	// The words before -- used to be dropped, and the command went to the
	// whole session set instead.
	h, err := run(t, harnessOptions{}, "exec", "exe0002", "--", "reboot")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	calls := h.recorder.Calls()
	if len(calls) != 1 || calls[0].Target.Name != "exe0002" {
		t.Errorf("calls = %+v, want one to exe0002", calls)
	}

	h, err = run(t, harnessOptions{}, "exec", "exe0002", "-r", "--script", "true")
	if err != nil {
		t.Fatalf("exec with a script failed: %v", err)
	}
	if calls := h.recorder.Calls(); len(calls) != 1 || calls[0].Target.Name != "exe0002" || calls[0].Target.User != "root" {
		t.Errorf("calls = %+v, want one to root@exe0002", calls)
	}

	// Words of the command before -- are not silently lost either: with
	// -n given, they are refused.
	for _, args := range [][]string{
		{"exec", "-n", "exe0001", "sudo", "--", "reboot"},
		{"exec", "-n", "exe0001", "reboot", "--", "now"},
		{"exec", "-n", "exe0001", "exe0002", "--", "reboot"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, args...)
			wantCode(t, err, exitcode.Usage)
			wantNoCalls(t, h)
		})
	}
}

// failingReader returns some data and then an error, as a file does that
// fails part way through.
type failingReader struct {
	data string
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("input/output error")
}

func TestExecStdinRefusesAFailedRead(t *testing.T) {
	script := []string{"exec", "--stdin", "-n", "exe[1-3]", "--script", "cat > /etc/motd"}

	t.Run("an error part way through", func(t *testing.T) {
		h, err := run(t, harnessOptions{in: &failingReader{data: "partial"}}, script...)
		if err == nil {
			t.Fatal("a failed read should stop the command")
		}
		if !strings.Contains(err.Error(), "input/output error") {
			t.Errorf("error = %v, want the read error", err)
		}
		wantCode(t, err, exitcode.Usage)
		wantNoCalls(t, h)
	})

	t.Run("a directory", func(t *testing.T) {
		dir, err := os.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		h, err := run(t, harnessOptions{in: dir}, script...)
		if err == nil {
			t.Fatal("reading a directory should stop the command")
		}
		wantNoCalls(t, h)
	})
}

// unreachable is what the ssh transport reports for a host it could not
// connect to.
func unreachable(tg transport.Target) *transport.Result {
	return &transport.Result{
		Target: tg, ExitCode: 255, Stderr: "ssh: connect to host: Connection refused\n",
		Err: exitcode.Wrap(exitcode.Transport, fmt.Errorf("%s: ssh: connect to host: Connection refused", tg)),
	}
}

// exited is what the ssh transport reports for a command that exited
// non-zero.
func exited(tg transport.Target, code int, stderr string) *transport.Result {
	return &transport.Result{
		Target: tg, ExitCode: code, Stderr: stderr,
		Err: fmt.Errorf("%s: command exited %d", tg, code),
	}
}

func TestExecExitCodeSaysWhatWentWrong(t *testing.T) {
	tests := []struct {
		name  string
		reply func(transport.Target) *transport.Result
		want  int
	}{
		{"a node refused", func(tg transport.Target) *transport.Result {
			if tg.Name == "exe0002" {
				return exited(tg, 1, "")
			}
			return &transport.Result{Target: tg}
		}, exitcode.TargetFailed},
		{"a node was unreachable", func(tg transport.Target) *transport.Result {
			if tg.Name == "exe0003" {
				return unreachable(tg)
			}
			return &transport.Result{Target: tg}
		}, exitcode.Transport},
		{"unreachable wins over refused", func(tg transport.Target) *transport.Result {
			switch tg.Name {
			case "exe0002":
				return exited(tg, 1, "")
			case "exe0003":
				return unreachable(tg)
			}
			return &transport.Result{Target: tg}
		}, exitcode.Transport},
		{"interrupted wins over everything", func(tg transport.Target) *transport.Result {
			switch tg.Name {
			case "exe0001":
				return exited(tg, 1, "")
			case "exe0002":
				return unreachable(tg)
			}
			return &transport.Result{Target: tg, ExitCode: -1, Err: context.Canceled}
		}, exitcode.Interrupted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
				return tc.reply(tg), nil
			}}
			for _, format := range []string{"table", "json"} {
				_, err := run(t, harnessOptions{recorder: rec}, "exec", "-o", format, "-n", "exe[1-3]", "--", "uptime")
				wantCode(t, err, tc.want)
			}
		})
	}

	// The error is kept, not its string, so that report() still sees the
	// cancellation.
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, ExitCode: -1, Err: context.Canceled}, nil
	}}
	_, err := run(t, harnessOptions{recorder: rec}, "exec", "-n", "exe[1-2]", "--", "uptime")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

func TestResultsTableShowsTheNodesOwnError(t *testing.T) {
	tg := transport.Target{Name: "exe0003", Host: "exe0003.hpc.example.org"}
	table := resultsTable([]*transport.Result{
		exited(tg, 3, "Unit slurmd.service could not be found.\n"),
		unreachable(transport.Target{Name: "exe0004"}),
	})
	rows := table.Objects()
	if got, want := rows[0]["error"], "Unit slurmd.service could not be found."; got != want {
		t.Errorf("ERROR = %q, want %q", got, want)
	}
	if got, want := rows[0]["status"], "exit 3"; got != want {
		t.Errorf("STATUS = %q, want %q", got, want)
	}
	if got, want := rows[1]["status"], "unreachable"; got != want {
		t.Errorf("STATUS = %q, want %q", got, want)
	}
	if !strings.Contains(rows[1]["error"], "Connection refused") {
		t.Errorf("ERROR = %q, want the connection failure", rows[1]["error"])
	}
}
