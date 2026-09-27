// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout

import (
	"cmp"
	"context"
	"errors"
	"io"

	pool "github.com/GSI-HPC/clusterctl/internal/clikit/fanout"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// program is the name the pools give clusterctl, in the line that says the
// work panicked and in the error it becomes.
const program = "clusterctl"

// DefaultMax is used when nothing configures the fan-out.
const DefaultMax = pool.DefaultMax

// Options say how Map works on its items and how it reports them, as
// internal/clikit/fanout has them.
type Options[T any] = pool.Options[T]

// Outcome is what the work for one item came to.
type Outcome[R any] = pool.Outcome[R]

// BatchOptions say how Batches splits a node set and reports its work.
type BatchOptions = pool.BatchOptions

// Batch is what came of one batch of Batches.
type Batch = pool.Batch

// ErrNotTried is the error of a batch that was not run because one before it
// failed.
var ErrNotTried = pool.ErrNotTried

// Each calls work with every index below n, at most limit at a time, as
// internal/clikit/fanout's Each does.
func Each(ctx context.Context, n, limit int, work func(i int)) {
	pool.Each(ctx, n, limit, work)
}

// Map is internal/clikit/fanout's Map as clusterctl runs it: a panic is
// clusterctl's, and the error it becomes asks for the exit code of a target
// that failed, as Recovered's does; the class of an error that says none of
// its own is that of its exit code; and the step ends with Summarize's
// error, which asks for the exit code the worst of the items' errors does.
// An Options that sets the program, the class or the summary keeps it.
func Map[T, R any](ctx context.Context, items []T, o Options[T], fn func(ctx context.Context, item T) (R, error)) []Outcome[R] {
	o.Program = cmp.Or(o.Program, program)
	if o.Classify == nil {
		o.Classify = exitcode.Class
	}
	if o.Summarize == nil {
		o.Summarize = func(n int, names []string, errs []error, interrupted bool) error {
			return Summarize("", n, names, errs, interrupted)
		}
	}
	out := pool.Map(ctx, items, o, fn)
	for i := range out {
		var p *pool.PanicError
		if errors.As(out[i].Err, &p) {
			out[i].Err = exitcode.Default(exitcode.TargetFailed, out[i].Err)
		}
	}
	return out
}

// Batches runs a node set in batches, as internal/clikit/fanout's Batches
// does.
func Batches(ctx context.Context, nodes *nodeset.NodeSet, o BatchOptions, run func(ctx context.Context, batch *nodeset.NodeSet) error) []Batch {
	return pool.Batches(ctx, nodes, o, run)
}

// Skip returns the error of work that leaves its item out on purpose, as
// internal/clikit/fanout's Skip does.
func Skip(reason string) error { return pool.Skip(reason) }

// IsSkipped reports whether err says that an item was left out on purpose.
func IsSkipped(err error) bool { return pool.IsSkipped(err) }

// Recovered turns a panic in the work for one target into that target's
// error, as internal/clikit/fanout's Recovered does for clusterctl, with the
// exit code of a target that failed.
func Recovered(log io.Writer, target string, v any) error {
	return exitcode.Wrap(exitcode.TargetFailed, pool.Recovered(log, program, target, v))
}

// Summarize is the error a fan-out of n that did not succeed everywhere
// exits with: internal/clikit/fanout's Failure, "k of n <noun> failed:
// <node set>", with the code exitcode.Worst gives the items' errors, or
// TargetFailed when none of them says, since a command that exited non-zero
// is a target that failed. Its progress class is that of the code, not of
// whichever of the items' errors says a class first, or canceled when
// interrupted says that the interrupt ended the items. It is nil when none
// failed.
func Summarize(noun string, n int, names []string, errs []error, interrupted bool) error {
	err := pool.Failure(noun, n, names, errs, interrupted)
	if err == nil {
		return nil
	}
	code := cmp.Or(exitcode.Worst(errs...), exitcode.TargetFailed)
	class := exitcode.CodeClass(code)
	if interrupted {
		class = progress.ClassCanceled
	}
	return &exitcode.Error{Code: code, Err: classed{err, class}}
}

// classed gives an error the progress class of its exit code.
type classed struct {
	error
	class progress.Class
}

func (c classed) Unwrap() error { return c.error }

// ProgressClass says why the fan-out failed as its exit code does.
func (c classed) ProgressClass() progress.Class { return c.class }

// FailureError turns the targets of a fan-out that failed into the error the
// command exits with, as Summarize makes it of the hosts that failed. The
// errors are kept underneath, not only their text, so that a caller can
// still tell a cancellation from a failure. It is nil when every target
// succeeded.
func FailureError(results []*transport.Result) error {
	var names []string
	var errs []error
	for _, r := range Failures(results) {
		names, errs = append(names, r.Target.Name), append(errs, r.Err)
	}
	return Summarize("hosts", len(results), names, errs, false)
}
