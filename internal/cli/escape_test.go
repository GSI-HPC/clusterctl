// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// raw lists what may not reach a terminal as it is: an escape sequence, a
// bell, a carriage return, a bidirectional override and isolate, the Unicode
// line separator and a byte that is not UTF-8.
var raw = []string{"\x1b", "\x07", "\r", "\u202e", "\u2066", "\u2028", "\xff"}

// wantNoRaw fails if a stream carries any of raw.
func wantNoRaw(t *testing.T, name, stream string) {
	t.Helper()
	for _, r := range raw {
		if strings.Contains(stream, r) {
			t.Errorf("%s carries %q as it is:\n%q", name, r, stream)
		}
	}
}

// Follow-up #79: exec escaped C0 and C1 controls but not the bidirectional
// controls or the line separator, so a node could still show its answer in
// another order than it has, or start what looks like a line of its own.
func TestExecEscapesBidiControlsAndLineSeparators(t *testing.T) {
	const evil = "ok\u202eevil\u2028exe0001: fine\u2066\n"
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if tg.Name == "exe0002" {
			return &transport.Result{Target: tg, Stdout: evil, Stderr: "bad\u2028worse\u202e\n", ExitCode: 1,
				Err: fmt.Errorf("%s: command exited 1", tg)}, nil
		}
		return &transport.Result{Target: tg, Stdout: "fine\n"}, nil
	}}
	for _, extra := range [][]string{nil, {"--dedup"}} {
		args := append(append([]string{"exec", "-n", "exe[1-2]"}, extra...), "--", "cat", "/etc/motd")
		h, _ := run(t, harnessOptions{recorder: rec}, args...)
		wantNoRaw(t, fmt.Sprintf("%v: stdout", extra), h.out.String())
		wantNoRaw(t, fmt.Sprintf("%v: stderr", extra), h.errOut.String())
		if want := `ok\u202eevil\u2028exe0001: fine\u2066`; !strings.Contains(h.out.String(), want) {
			t.Errorf("%v: output = %q, want %q", extra, h.out, want)
		}
		if want := `bad\u2028worse\u202e`; !strings.Contains(h.errOut.String(), want) {
			t.Errorf("%v: stderr = %q, want %q", extra, h.errOut, want)
		}
	}
}

// Follow-up #79: the group resolver had an escaper of its own, which left the
// bidirectional controls, the line separator and bytes that are not UTF-8 as
// they were.
func TestFailingGroupCommandEscapesItsMessage(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{
			Target: tg, ExitCode: 1,
			Stderr: "sinfo: \u202edenied\u2028\u2066 \xff\x1b[2J\n",
			Err:    fmt.Errorf("%s: command exited 1", tg),
		}, nil
	}}
	h, code := exitCodeOf(t, harnessOptions{recorder: rec}, "node", "select", "@slurm:main")
	if code != exitcode.TargetFailed {
		t.Errorf("exit code = %d, want %d", code, exitcode.TargetFailed)
	}
	wantNoRaw(t, "stderr", h.errOut.String())
	if want := `sinfo: \u202edenied\u2028\u2066 \xff\x1b[2J`; !strings.Contains(h.errOut.String(), want) {
		t.Errorf("stderr = %q, want %q", h.errOut, want)
	}
}
