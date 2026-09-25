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

// Follow-up #79: report printed every error with %v, so text an error quoted
// from a node, a BMC or a group source reached the terminal as it was.
func TestReportEscapesTheError(t *testing.T) {
	const osc52 = "\x1b]52;c;Zm9v\x07"
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return nil, exitcode.Errorf(exitcode.Transport, "%s: ssh: %s\u202e\r", tg, osc52)
	}}
	h, code := exitCodeOf(t, harnessOptions{recorder: rec}, "node", "select", "@slurm:main")
	if code != exitcode.Transport {
		t.Errorf("exit code = %d, want %d", code, exitcode.Transport)
	}
	wantNoRaw(t, "stderr", h.errOut.String())
	if want := `ssh: \x1b]52;c;Zm9v\x07\u202e\r`; !strings.Contains(h.errOut.String(), want) {
		t.Errorf("stderr = %q, want %q", h.errOut, want)
	}
}

// The escape keeps the lines clusterctl breaks a message into, such as the
// list of problems of a configuration file, and keeps the exit code, also
// when the command was interrupted.
func TestReportKeepsLinesAndExitCode(t *testing.T) {
	err := exitcode.Wrap(exitcode.Usage,
		errors.New("site.yaml is not valid:\n  first \x1b[2J\n  second\u2028"))
	var out strings.Builder
	h, _ := build(t, harnessOptions{})
	streams := h.streams
	streams.Err = &out
	if got, want := report(context.Background(), streams, err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if want := "clusterctl: site.yaml is not valid:\n  first \\x1b[2J\n  second\\u2028\n"; out.String() != want {
		t.Errorf("stderr = %q, want %q", out.String(), want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	if got, want := report(ctx, streams, errors.New("bmc: \x1b]0;title\x07")), exitcode.Interrupted; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if want := "clusterctl: interrupted: bmc: \\x1b]0;title\\x07\n"; out.String() != want {
		t.Errorf("stderr = %q, want %q", out.String(), want)
	}
}

// Follow-up #79: boot sync, boot log and fabric counters printed what the
// infrastructure host answered as it was. The PXE log records what booting
// nodes asked for, and a node's root sets the description the fabric tools
// print.
func TestInfrastructureOutputIsEscaped(t *testing.T) {
	const evil = "first \x1b]52;c;Zm9v\x07\u202e\nsecond\r\u2028\n"
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		out := evil
		switch {
		case len(req.Argv) > 0 && req.Argv[0] == "ibaddr":
			out = "GID fe80::11:2203:33:4455 LID start 0x5 end 0x5\n"
		case strings.Contains(strings.Join(req.Argv, " "), "dhcpd.conf"):
			// fabric counters reads the DHCP configuration first.
			out = ""
		}
		return &transport.Result{Target: tg, Stdout: out}, nil
	}}
	for _, args := range [][]string{
		{"boot", "sync", "-y"},
		{"boot", "log"},
		{"fabric", "counters", "exe0001"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{recorder: rec}, args...)
			if err != nil {
				t.Fatalf("failed: %v", err)
			}
			wantNoRaw(t, "stdout", h.out.String())
			// The lines the host printed are kept.
			if want := "first \\x1b]52;c;Zm9v\\x07\\u202e\nsecond\\r\\u2028\n"; !strings.Contains(h.out.String(), want) {
				t.Errorf("stdout = %q, want %q", h.out, want)
			}
		})
	}
}

// Follow-up #79: the default account sacctmgr reports for a user reached the
// preview of slurm user add as it was.
func TestSlurmUserAddPreviewEscapesWhatSlurmSaid(t *testing.T) {
	cluster := newSlurmCluster()
	cluster.associations = "alice|other|oth\x1b[2J\u202eer|1\n"
	h, _ := run(t, harnessOptions{recorder: cluster.recorder()},
		"slurm", "user", "add", "alice", "proj", "proj", "--dry-run")
	wantNoRaw(t, "stderr", h.errOut.String())
	if want := `instead of oth\x1b[2J\u202eer`; !strings.Contains(h.errOut.String(), want) {
		t.Errorf("the preview says:\n%q\nwant %q", h.errOut, want)
	}
}
