// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/safety"
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
// one, the command gets the Bus of its own display, if --progress has it
// drawn, which is closed, and the display taken off the terminal, before
// the function's error is printed by report.
//
// The span's context becomes the tree's before the command context is
// built, so a.Context(), the gate and the group resolver all carry it, and
// everything the command reports nests under it.
func (r *root) inSpan(cmd *cobra.Command, args []string, run func(*cobra.Command, []string) error) (err error) {
	interrupt := r.context()
	ctx := interrupt
	if progress.BusFrom(ctx) == nil && !r.agent {
		bus, done, refused := r.display(cmd)
		if refused != nil {
			return refused
		}
		if bus != nil {
			ctx = progress.WithBus(ctx, bus)
			defer func() {
				bus.Close()
				done()
			}()
		}
	}
	var flags progress.Flags
	if r.dryRun {
		flags = progress.DryRun
	}
	ctx, span := progress.Start(ctx, progress.KindCommand, commandPath(cmd), progress.WithFlags(flags))
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
// flags given, and ends the step with what fn returned: progress.Hidden
// for the lookups a command makes before it asks or changes anything.
func inStep(ctx context.Context, name string, flags progress.Flags, fn func(context.Context) error) error {
	ctx, step := progress.Start(ctx, progress.KindStep, name, progress.WithFlags(flags))
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

// The ways --progress shows the progress of a command.
const (
	// progressAuto is the counter where it can be drawn, and nothing
	// elsewhere.
	progressAuto = "auto"
	// progressCounter is one line on standard error, redrawn as the work
	// goes on.
	progressCounter = "counter"
	// progressNone shows nothing: standard error carries what it would
	// without progress, byte for byte.
	progressNone = "none"
)

// progressModes are the values --progress takes.
var progressModes = []string{progressAuto, progressCounter, progressNone}

// progressMode returns how a command's progress is shown: as --progress
// says, else CLUSTERCTL_PROGRESS, else auto, which draws the counter where
// it can be drawn, on a standard error that is a terminal and not a dumb
// one, and shows nothing elsewhere: in a pipe, a file or a script's
// capture, standard error is left as it would be without progress. Nor
// does auto draw while standard output goes into a pipe, since what reads
// it, grep or less, writes to the same terminal, over the counter's line,
// and nothing keeps the two apart; asked for, the counter is drawn all the
// same. A counter --progress asks for where it cannot be drawn is refused
// rather than dropped, so that the one who asked finds out why nothing
// shows.
//
// The variable is set once, in a profile, and inherited by the cron jobs
// and CI steps whose standard error is no terminal, which never asked for
// anything: what it asks for fails no command. The counter it asks for
// where none can be drawn shows nothing, as auto would, and a value it does
// not take shows nothing either, with the note returned to say why, for
// standard error.
func (r *root) progressMode() (mode, note string, err error) {
	mode, from := r.progress, "--progress"
	ambient := r.progressGiven == nil || !r.progressGiven()
	if ambient {
		mode, from = os.Getenv(config.EnvProgress), config.EnvProgress
		if mode == "" {
			mode = progressAuto
		}
	}
	unfit := ""
	switch {
	case !r.streams.ErrIsTTY:
		unfit = "standard error is not a terminal"
	case os.Getenv("TERM") == "dumb":
		unfit = "the terminal cannot draw one (TERM is dumb)"
	}
	switch mode {
	case progressAuto:
		if unfit != "" || r.streams.OutIsPipe {
			return progressNone, "", nil
		}
		return progressCounter, "", nil
	case progressCounter:
		if unfit == "" {
			return progressCounter, "", nil
		}
		if ambient {
			return progressNone, "", nil
		}
		return "", "", exitcode.Errorf(exitcode.Usage,
			"%s asks for a counter, but %s; use none, or auto to draw one only where it can be", from, unfit)
	case progressNone:
		return mode, "", nil
	}
	unknown := fmt.Sprintf("%s is %q; it takes one of %s", from, mode, strings.Join(progressModes, ", "))
	if ambient {
		return progressNone, "clusterctl: " + unknown + "; no progress is shown", nil
	}
	return "", "", exitcode.Errorf(exitcode.Usage, "%s", unknown)
}

// startCounter makes the counter a command's progress is drawn as, on term,
// and starts drawing it. The tests draw the frames themselves, on a clock of
// their own.
var startCounter = func(term *display.Terminal) *display.Counter {
	counter := display.NewCounter(term, display.CounterOptions{})
	counter.Start()
	return counter
}

// display returns the Bus a command's progress is drawn from when nothing
// watches the command already, and what takes the display off the terminal
// and puts the streams back once the Bus is closed; no Bus when --progress
// has nothing drawn. An agent's commands never draw; the MCP server gives
// them a Bus of its own.
//
// Everything the command writes where the display draws goes through the
// writers of its terminal, which take the display off first: standard
// error, the diagnostics, and standard output when it shows on the terminal
// too, of the command context and of cobra alike, whose output printExec
// and the help write to. They are put in place before the command context
// is built, which copies the streams, and only while a display is drawn:
// without one, nothing stands between the command and its streams.
func (r *root) display(cmd *cobra.Command) (*progress.Bus, func(), error) {
	mode, modeNote, err := r.progressMode()
	if modeNote != "" {
		report := r.streams.Diag
		if report == nil {
			report = r.streams.Err
		}
		// A note that cannot be written changes nothing about the command.
		_, _ = fmt.Fprintln(report, output.EscapeCell(modeNote))
	}
	if err != nil || mode == progressNone {
		return nil, nil, err
	}
	term := display.NewTerminal(r.streams.Err, r.streams.Size)
	term.PanicLog, term.Foreground = r.streams.Diag, r.streams.Foreground
	if term.PanicLog == nil {
		term.PanicLog = r.streams.Err
	}
	counter := startCounter(term)

	saved, top := r.streams, cmd.Root()
	out, errOut := top.OutOrStdout(), top.ErrOrStderr()
	if r.streams.OutIsTTY {
		r.streams.Out = term.Writer(r.streams.Out)
		top.SetOut(term.Writer(out))
	}
	r.streams.Err = term.Writer(r.streams.Err)
	top.SetErr(term.Writer(errOut))
	diag := r.streams.Err
	if r.streams.Diag != nil {
		r.streams.Diag = term.Writer(r.streams.Diag)
		diag = r.streams.Diag
	}
	r.streams.Display = true

	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{counter}, PanicLog: diag})
	return bus, func() {
		counter.Close()
		r.streams = saved
		top.SetOut(out)
		top.SetErr(errOut)
	}, nil
}
