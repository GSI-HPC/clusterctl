// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
	// progressAuto is the live tree where it can be drawn, and nothing
	// elsewhere.
	progressAuto = "auto"
	// progressTTY is the live tree: a few rows at the bottom of the
	// terminal, redrawn as the work goes on, or the counter's line on a
	// terminal too small for them.
	progressTTY = "tty"
	// progressCounter is one line on standard error, redrawn as the work
	// goes on.
	progressCounter = "counter"
	// progressPlain is a line on standard error for each thing worth one,
	// with no escape codes, for a log as much as a terminal.
	progressPlain = "plain"
	// progressNone shows nothing: standard error carries what it would
	// without progress, byte for byte.
	progressNone = "none"
)

// progressModes are the values --progress takes.
var progressModes = []string{progressAuto, progressTTY, progressCounter, progressPlain, progressNone}

// progressMode returns how a command's progress is shown: as --progress
// says, else CLUSTERCTL_PROGRESS, else auto, which draws the live tree where
// it can be drawn, on a standard error that is a terminal and not a dumb
// one, and shows nothing elsewhere: in a pipe, a file or a script's
// capture, standard error is left as it would be without progress. Nor
// does auto draw while standard output goes into a pipe, since what reads
// it, grep or less, writes to the same terminal, over the tree and under
// it, and nothing keeps the two apart; asked for, the tree is drawn all
// the same. The tree or the counter --progress asks for where it cannot be
// drawn is refused rather than dropped, so that the one who asked finds
// out why nothing shows. Plain lines need no terminal, and are shown
// wherever they are asked for.
//
// The variable is set once, in a profile, and inherited by the cron jobs
// and CI steps whose standard error is no terminal, which never asked for
// anything: what it asks for fails no command. The tree or the counter it
// asks for where neither can be drawn shows nothing, as auto would, and a
// value it does not take shows nothing either, with the note returned to
// say why, for standard error.
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
		return progressTTY, "", nil
	case progressTTY, progressCounter:
		if unfit == "" {
			return mode, "", nil
		}
		if ambient {
			return progressNone, "", nil
		}
		what := "a live tree"
		if mode == progressCounter {
			what = "a counter"
		}
		return "", "", exitcode.Errorf(exitcode.Usage,
			"%s asks for %s, but %s; use none, or auto to draw one only where it can be", from, what, unfit)
	case progressPlain, progressNone:
		return mode, "", nil
	}
	unknown := fmt.Sprintf("%s is %q; it takes one of %s", from, mode, strings.Join(progressModes, ", "))
	if ambient {
		return progressNone, "clusterctl: " + unknown + "; no progress is shown", nil
	}
	return "", "", exitcode.Errorf(exitcode.Usage, "%s", unknown)
}

// utf8Locale reports whether the locale the environment names, through
// getenv, writes text as UTF-8, which the marks of the live tree need:
// LC_ALL, else LC_CTYPE, else LANG, the way the C library reads them. With
// none of them set the locale is C, and the tree is drawn in ASCII.
func utf8Locale(getenv func(string) string) bool {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if locale := strings.ToLower(getenv(name)); locale != "" {
			return strings.Contains(locale, "utf-8") || strings.Contains(locale, "utf8")
		}
	}
	return false
}

// renderer is a display a command's progress is shown by: a sink of its
// Bus, closed once the Bus is.
type renderer interface {
	progress.Sink
	Close()
}

// startDisplay makes the display a command's progress is shown by in mode,
// on term, and starts it; interrupted is closed once the command has been
// interrupted, which the tree says. The tests replace it to draw the frames
// themselves, on a clock of their own that they make displayClock too.
var startDisplay = func(mode string, term *display.Terminal, interrupted <-chan struct{}) renderer {
	ascii := !utf8Locale(os.Getenv)
	switch mode {
	case progressPlain:
		plain := display.NewPlain(term, display.PlainOptions{Now: displayClock, ASCII: ascii})
		plain.Start()
		return plain
	case progressTTY:
		tree := display.NewTree(term, display.TreeOptions{Now: displayClock, ASCII: ascii, Interrupted: interrupted})
		tree.Start()
		return tree
	}
	counter := display.NewCounter(term, display.CounterOptions{Now: displayClock, ASCII: ascii})
	counter.Start()
	return counter
}

// displayClock is the clock of a display's Bus.
var displayClock = time.Now

// display returns the Bus a command's progress is shown from when nothing
// watches the command already, and what takes the display off the terminal,
// puts the streams back and leaves the summary once the Bus is closed; no
// Bus when --progress has nothing shown. An agent's commands never show
// anything; the MCP server gives them a Bus of its own.
//
// Everything the command writes where the display shows goes through the
// writers of its terminal, which take the tree or the counter off first,
// and write the lines of the steps that finished, or the plain lines, that
// came before: standard error, the diagnostics, and standard output when it
// shows on the terminal too, or wherever it goes for plain lines, which a
// log may keep along with the output; those of the
// command context and cobra's alike, which printExec and the help write to.
// They are put in place before the command context is built, which copies
// the streams, and only while a display is shown: without one, nothing
// stands between the command and its streams.
//
// The summary is one line on standard error once the display is gone, and
// before the command's error, for a command that ran for a second or more:
// how many of its targets ended how, and how long it ran. It says it is
// clusterctl's, as the error does, so that it is not read as a host's. What
// failed and why is the error's to say.
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
	shown := startDisplay(mode, term, r.context().Done())
	summary := &display.Summary{}

	saved, top := r.streams, cmd.Root()
	out, errOut := top.OutOrStdout(), top.ErrOrStderr()
	if r.streams.OutIsTTY || mode == progressPlain {
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

	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{shown, summary}, Now: displayClock, PanicLog: diag})
	return bus, func() {
		shown.Close()
		r.streams = saved
		top.SetOut(out)
		top.SetErr(errOut)
		if line := summary.Line(); line != "" {
			// The summary is a courtesy; a line that cannot be written
			// changes nothing about the command.
			_, _ = fmt.Fprintln(saved.Err, "clusterctl: "+line)
		}
	}, nil
}
