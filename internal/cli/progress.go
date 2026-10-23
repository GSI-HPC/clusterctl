// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/GSI-HPC/go-clikit/progress/display"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/version"
)

// traceLeaves has every leaf command of the tree run in a progress span of
// its own, the root of what it reports: the commands r.run builds and those
// with a function of their own alike, cobra's help and completion among
// them.
//
// An interactive command is left as it is. It hands the terminal to another
// program, a shell, a browser or sshuttle, or keeps running, as mcp serve
// does, until it is stopped; nothing may be drawn while it runs, and a span
// open for the length of a session would say nothing a display could use.
func traceLeaves(cmd *cobra.Command, r *root) {
	for _, sub := range cmd.Commands() {
		traceLeaves(sub, r)
	}
	if cmd.HasSubCommands() || safety.EffectOf(cmd.Annotations) == safety.EffectInteractive {
		return
	}
	run := cmd.RunE
	if run == nil && cmd.Run != nil {
		plain := cmd.Run
		run = func(c *cobra.Command, args []string) error {
			plain(c, args)
			return nil
		}
		cmd.Run = nil
	}
	if run == nil {
		return
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		return r.inSpan(c, args, run)
	}
}

// errPanicked ends the span of a command whose function panicked.
var errPanicked = errors.New("the command panicked")

// inSpan runs the function of a leaf command in the command's span, named
// by its path and marked as a dry run's under --dry-run, and ends the span
// with what the function returned, as report reads it: a dry run the gate
// stopped on purpose ends well, and a command that failed once it had been
// interrupted ends canceled, whatever its error says.
//
// The span is started under the Bus the command tree's context brought,
// which an MCP call or a test gives it; the Bus is theirs to close. Without
// one, the command gets a Bus of its own when --progress has a display
// drawn or --progress-log an event log written, which is closed, the
// display taken off the terminal and the log written out, before the
// function's error is printed by report.
//
// The span's context becomes the tree's before the command context is
// built, so a.Context(), the gate and the group resolver all carry it, and
// everything the command reports nests under it.
func (r *root) inSpan(cmd *cobra.Command, args []string, run func(*cobra.Command, []string) error) (err error) {
	interrupt := r.context()
	ctx := interrupt
	if progress.BusFrom(ctx) == nil && !r.agent {
		shown, done, refused := r.display(cmd)
		if refused != nil {
			return refused
		}
		ctx = shown
		defer done()
	}
	var flags progress.Flags
	if r.dryRun {
		flags = progress.FlagDryRun
	}
	ctx, span := progress.Start(ctx, progress.KindCommand, commandPath(cmd), progress.Flagged(flags))
	returned := false
	defer func() {
		switch {
		case !returned:
			span.End(errPanicked)
		case safety.IsDryRun(err):
			span.End(nil)
		case err != nil && interrupt.Err() != nil:
			span.End(interrupted{err})
		default:
			span.End(err)
		}
	}()
	r.ctx = ctx
	err = run(cmd, args)
	returned = true
	return err
}

// inStep runs fn as a step of the work ctx carries, named name, with the
// flags given, and ends the step with what fn returned: progress.FlagHidden
// for the lookups a command makes before it asks or changes anything.
func inStep(ctx context.Context, name string, flags progress.Flags, fn func(context.Context) error) error {
	ctx, step := progress.Start(ctx, progress.KindStep, name, progress.Flagged(flags))
	err := fn(ctx)
	step.End(err)
	return err
}

// interrupted is the error of a command that failed once it had been
// interrupted, which is what ended it, as report tells: its class is
// canceled, and its text the command's error.
type interrupted struct{ error }

func (e interrupted) Unwrap() error { return e.error }

// ProgressClass says that the command was interrupted.
func (interrupted) ProgressClass() progress.Class { return progress.ClassCanceled }

// progressModes are the values --progress takes.
var progressModes = func() []string {
	var words []string
	for _, mode := range cliprogress.Modes() {
		words = append(words, mode.String())
	}
	return words
}()

// hostCount says how many targets a step expects in the plain lines, as
// clusterctl names them: "1 host" and "480 hosts".
func hostCount(n int) string {
	if n == 1 {
		return "1 host"
	}
	return fmt.Sprintf("%d hosts", n)
}

// takeTraceContext returns the W3C trace context clusterctl was started
// with, TRACEPARENT and TRACESTATE, and takes both out of its environment.
// The event log continues the trace; the programs clusterctl runs, ssh,
// scp, sops and a credential helper, are handed none. clusterctl sends its
// spans nowhere, so a child told to run under the caller's span, or under
// one of clusterctl's, would hang what it traces under a parent no tracing
// system holds.
func takeTraceContext() (traceparent, tracestate string) {
	for _, name := range []string{"TRACEPARENT", "TRACESTATE"} {
		value, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		// Unsetenv fails only for a name the system cannot hold.
		_ = os.Unsetenv(name)
		if name == "TRACEPARENT" {
			traceparent = value
		} else {
			tracestate = value
		}
	}
	return traceparent, tracestate
}

// defaultLogLimit is the size at which the default event log is moved aside,
// so that the log and its copy take up about twice this at most.
const defaultLogLimit = 8 << 20

// defaultLog returns the event log every command appends to when neither
// --progress-log nor CLUSTERCTL_PROGRESS_LOG names one: activity.jsonl in
// the state directory. CLUSTERCTL_PROGRESS_LOG set to nothing asks for no
// log, so it leaves none. Like the state directory, the file is the user's
// alone.
func (r *root) defaultLog() string {
	if _, set := os.LookupEnv(config.EnvProgressLog); set || r.streams.StateDir == "" {
		return ""
	}
	// A state directory that cannot be made leaves the log out, as the
	// default log is never worth a complaint.
	if err := os.MkdirAll(r.streams.StateDir, 0o700); err != nil {
		return ""
	}
	return filepath.Join(r.streams.StateDir, "activity.jsonl")
}

// startProgress starts the progress of a command; the tests replace it to
// draw the frames themselves, on a clock of their own.
var startProgress = cliprogress.Start

// progressOptions returns what cliprogress needs to know of the program, its
// flags, its environment and its streams, to show the progress of cmd.
//
// Everything the command writes where the display shows goes through the
// writers of the display, which take the tree or the counter off first,
// and write the lines of the steps that finished, or the plain lines, that
// came before: standard error, the diagnostics, and standard output when it
// shows on the terminal too, or wherever it goes for plain lines, which a
// log may keep along with the output.
func (r *root) progressOptions() cliprogress.Options {
	diag := r.streams.Diag
	if diag == nil {
		diag = r.streams.Err
	}
	// What goroutines beside the command write, the panics of a pool's
	// workers and of the Bus's sinks, which may come while a question is
	// asked, goes to the diagnostics as it would.
	panicLog := r.streams.WorkerDiag
	if panicLog == nil {
		panicLog = diag
	}
	return cliprogress.Options{
		Program: "clusterctl",
		Mode: cliprogress.Setting{
			Flag: "--progress", Given: r.progressGiven != nil && r.progressGiven(), Value: r.progress,
			Variable: config.EnvProgress, Env: os.Getenv(config.EnvProgress),
		},
		Log: cliprogress.Setting{
			Flag: "--progress-log", Given: r.progressLogGiven != nil && r.progressLogGiven(), Value: r.progressLog,
			Variable: config.EnvProgressLog, Env: os.Getenv(config.EnvProgressLog),
		},
		DefaultLog: r.defaultLog(),
		RotateAt:   defaultLogLimit,
		LogOptions: progress.LogOptions{Version: version.Get().Version},
		Stderr:     r.streams.Err,
		Notes:      diag,
		OnTerminal: r.streams.ErrIsTTY,
		Dumb:       os.Getenv("TERM") == "dumb",
		IntoPipe:   r.streams.OutIsPipe,
		Size:       r.streams.Size,
		Foreground: r.streams.Foreground,
		ASCII:      !cliprogress.UTF8Locale(os.Getenv),
		Theme:      cliprogress.ThemeFor(display.Classic, r.streams.ErrIsTTY, os.Getenv),
		Noun:       hostCount,
		Trace: func() progress.TraceContext {
			tc, _ := progress.ParseTraceContext(r.traceparent, r.tracestate)
			return tc
		},
		Classify: exitcode.Class,
		PanicLog: panicLog,
	}
}

// display starts the progress of a command when nothing watches the
// command already, and returns the context it runs under, and what takes
// the display off the terminal, puts the streams back, leaves the summary
// and closes the event log once the command's span has ended. Without a
// display or an event log the context is the tree's own. An agent's
// commands never get one; the MCP server gives them a Bus of its own.
//
// The writers in front of the streams are put in place before the command
// context is built, which copies the streams, and only while a display is
// shown: without one, nothing stands between the command and its streams,
// event log or not. What goroutines beside the command write goes through
// the display's Lines instead, which holds it while a question is asked, a
// password among them, and writes it once the question has been answered.
//
// The summary is one line on standard error once the display is gone, and
// before the command's error, for a command that ran for a second or more:
// how many of its targets ended how, and how long it ran. It says it is
// clusterctl's, as the error does, so that it is not read as a host's. What
// failed and why is the error's to say.
func (r *root) display(cmd *cobra.Command) (context.Context, func(), error) {
	opts := r.progressOptions()
	run, err := startProgress(r.context(), opts)
	if err != nil {
		return nil, nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if run.Mode() == cliprogress.ModeNone {
		return run.Context(), func() { run.Finish(true) }, nil
	}
	saved := r.streams
	top := cmd.Root()
	out, errOut := top.OutOrStdout(), top.ErrOrStderr()
	if r.streams.OutIsTTY || run.Mode() == cliprogress.ModePlain {
		r.streams.Out = run.Writer(r.streams.Out)
		top.SetOut(run.Writer(out))
	}
	r.streams.Err = run.Writer(r.streams.Err)
	top.SetErr(run.Writer(errOut))
	if r.streams.Diag != nil {
		r.streams.Diag = run.Writer(r.streams.Diag)
	}
	r.streams.WorkerDiag = run.Lines(opts.PanicLog)
	r.streams.Display = true
	return run.Context(), func() {
		run.Finish(true)
		r.streams = saved
		top.SetOut(out)
		top.SetErr(errOut)
	}, nil
}
