// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/progress"
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
// one, the command gets the Bus of its own display, which is closed, and
// the display taken off the terminal, before the function's error is
// printed by report.
//
// The span's context becomes the tree's before the command context is
// built, so a.Context(), the gate and the group resolver all carry it, and
// everything the command reports nests under it.
func (r *root) inSpan(cmd *cobra.Command, args []string, run func(*cobra.Command, []string) error) (err error) {
	interrupt := r.context()
	ctx := interrupt
	if progress.BusFrom(ctx) == nil && !r.agent {
		if bus, done := r.display(cmd); bus != nil {
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

// interrupted is the error of a command that failed once it had been
// interrupted, which is what ended it, as report tells: its class is
// canceled, and its text the command's error.
type interrupted struct{ error }

func (e interrupted) Unwrap() error { return e.error }

// ProgressClass says that the command was interrupted.
func (interrupted) ProgressClass() progress.Class { return progress.ClassCanceled }

// display returns the Bus a command's progress is drawn from when nothing
// watches the command already, and what puts the terminal back once the
// Bus is closed. There is no display yet, so there is no Bus either: the
// spans of a command go nowhere unless its context brought a Bus. An
// agent's commands never draw; the MCP server gives them its own.
func (r *root) display(*cobra.Command) (*progress.Bus, func()) {
	return nil, nil
}
