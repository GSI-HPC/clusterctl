// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func wantCode(t *testing.T, err error, want int) {
	t.Helper()
	if got := exitcode.From(err); got != want {
		t.Errorf("exit code = %d (%v), want %d", got, err, want)
	}
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
