// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout

import (
	"cmp"
	"context"
	"errors"
	"io"

	pool "github.com/GSI-HPC/go-clikit/fanout"
	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-nodeset"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// program is the name the pools give clusterctl, in the line that says the
// work panicked and in the error it becomes.
const program = "clusterctl"

// DefaultLimit is used when nothing configures the fan-out.
const DefaultLimit = pool.DefaultLimit

// MapOptions say how Map works on its items and how it reports them, as
// go-clikit's fanout has them.
type MapOptions[T any] = pool.MapOptions[T]

// Item is what MapOptions.Describe says of an item.
type Item = pool.Item

// Outcome is what the work for one item came to.
type Outcome[R any] = pool.Outcome[R]

// Summary is what the items of a fan-out came to, as Summarize is given it.
type Summary = pool.Summary

// Failed is one item that failed, as Summary lists it.
type Failed = pool.Failed

// BatchOptions say how Batches splits a node set and reports its work.
type BatchOptions = pool.BatchOptions

// Batch is what came of one batch of Batches.
type Batch = pool.Batch

// ErrNotTried is the error of a batch that was not run because one before it
// failed.
var ErrNotTried = pool.ErrNotTried

// Each calls work with every index below n, at most limit at a time, as
// go-clikit's fanout.Each does.
func Each(ctx context.Context, n, limit int, work func(i int)) {
	pool.Each(ctx, n, limit, work)
}

// Map is go-clikit's fanout.Map as clusterctl runs it: a panic is
// clusterctl's, and the error it becomes asks for the exit code of a target
// that failed, as Recovered's does; the class of an error that says none of
// its own is that of its exit code; and the step ends with Summarize's
// error, which names the items by o.Noun and asks for the exit code the
// worst of the items' errors does, and which Map returns as well. A
// MapOptions that sets the program, the class or the summary keeps it.
func Map[T, R any](ctx context.Context, items []T, o MapOptions[T], fn func(ctx context.Context, item T) (R, error)) ([]Outcome[R], error) {
	o.Program = cmp.Or(o.Program, program)
	if o.Classify == nil {
		o.Classify = exitcode.Class
	}
	if o.Summarize == nil {
		noun := o.Noun
		o.Summarize = func(s Summary) error { return Summarize(noun, s) }
	}
	out, err := pool.Map(ctx, items, o, fn)
	for i := range out {
		var p *pool.PanicError
		if errors.As(out[i].Err, &p) {
			out[i].Err = exitcode.Default(exitcode.TargetFailed, out[i].Err)
		}
	}
	return out, err
}

// Batches runs a node set in batches, as go-clikit's fanout.Batches does.
func Batches(ctx context.Context, nodes *nodeset.NodeSet, o BatchOptions, run func(ctx context.Context, batch *nodeset.NodeSet) error) []Batch {
	return pool.Batches(ctx, nodes, o, run)
}

// Recovered turns a panic in the work for one target into that target's
// error, as go-clikit's fanout.Recovered does for clusterctl, with the exit
// code of a target that failed.
func Recovered(log io.Writer, target string, v any) error {
	return exitcode.Wrap(exitcode.TargetFailed, pool.Recovered(log, program, target, v))
}

// Summarize is the error a fan-out that did not succeed everywhere exits
// with: go-clikit's fanout.Failure, "k of n <noun> failed: <node set>", with
// the code exitcode.Worst gives the items' errors, or TargetFailed when none
// of them says, since a command that exited non-zero is a target that
// failed. Its progress class is that of the code, not of whichever of the
// items' errors says a class first, or canceled when s.Canceled says that
// every item that failed ended canceled. It is nil when none failed.
func Summarize(noun string, s Summary) error {
	err := pool.Failure(noun, s)
	if err == nil {
		return nil
	}
	errs := make([]error, len(s.Failed))
	for i, f := range s.Failed {
		errs[i] = f.Err
	}
	code := cmp.Or(exitcode.Worst(errs...), exitcode.TargetFailed)
	class := exitcode.CodeClass(code)
	if s.Canceled {
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
	s := Summary{Total: len(results)}
	for _, r := range Failures(results) {
		s.Failed = append(s.Failed, Failed{Name: r.Target.Name, Err: r.Err})
	}
	return Summarize("hosts", s)
}
