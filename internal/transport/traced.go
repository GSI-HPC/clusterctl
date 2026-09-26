// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport

import (
	"context"
	"fmt"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// Traced returns a Runner that reports every request next runs as a call,
// "ssh", under the span its context carries: the target of a pool, a step,
// or the command itself. The call carries the node, the host, the role and
// the request's timeout, and ends with the command's exit status and the
// error it failed with, which says why it failed in a word: a host that
// could not be reached, a command that ran out of time or one that exited
// non-zero. The context next is given carries the call, so that what next
// reports of its own nests under it.
//
// A dry run's recorder is traced with dryRun set: what it was given was
// recorded, not sent, so its calls are marked as a dry run's and end
// skipped. The real transport of a dry run's lookups is traced without it,
// since those reach the host.
//
// Without a Bus in the context, nothing is reported, and the request runs
// as it would untraced.
func Traced(next Runner, dryRun bool) Runner {
	return traced{next: next, dryRun: dryRun}
}

type traced struct {
	next   Runner
	dryRun bool
}

// notSent is how a dry run's call ends.
const notSent = "dry run: not sent"

func (t traced) Run(ctx context.Context, target Target, req Request) (*Result, error) {
	if progress.BusFrom(ctx) == nil {
		return t.next.Run(ctx, target, req)
	}
	flags := progress.Flags(0)
	if t.dryRun {
		flags = progress.DryRun
	}
	ctx, call := progress.Start(ctx, progress.KindCall, "ssh", progress.WithFlags(flags),
		progress.Node(target.Name), progress.Host(target.Host), progress.Role(target.Role),
		progress.Timeout(req.Timeout))
	result, err := t.next.Run(ctx, target, req)
	switch {
	case err != nil:
		call.End(err)
	case t.dryRun:
		call.Skip(notSent)
	case result == nil:
		call.End(fmt.Errorf("%s: no result", target))
	case result.ExitCode >= 0:
		call.End(callError(target, result), progress.Exit(result.ExitCode))
	default:
		call.End(callError(target, result))
	}
	return result, err
}

// callError is the error a call ends with: the result's own, or its exit
// status when it failed without one, as a stand-in for the transport may
// answer.
func callError(target Target, r *Result) error {
	if r.Err != nil || r.ExitCode == 0 {
		return r.Err
	}
	return fmt.Errorf("%s: command exited %d", target, r.ExitCode)
}
