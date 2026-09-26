// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout

import (
	"cmp"
	"context"
	"fmt"
	"io"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Options say how Map works on its items and how it reports them.
type Options[T any] struct {
	// Step names the step a display shows the items under, such as
	// "copy" or "reset the machines".
	Step string
	// Flags are given to the step, which is Fold as well: ShowLines for
	// work whose output is the product.
	Flags progress.Flags
	// Limit is how many items are worked on at once; below one is
	// DefaultMax.
	Limit int
	// Describe says what a display names an item by: the node, or
	// whatever else the item is, the host the work goes to and its role.
	// Nil names an item the way fmt.Sprint prints it.
	Describe func(T) (node, host, role string)
	// PanicLog receives the stack of a panic in the work, the front end's
	// diagnostics; nil is the process's standard error.
	PanicLog io.Writer
}

// Outcome is what the work for one item came to.
type Outcome[R any] struct {
	// Value is what the work returned; the zero value when it panicked.
	Value R
	// Err is the error the work returned, or the one a panic in it
	// became. For an item that was never started it is the context's.
	Err error
	// Started says whether the work for the item was started, which it
	// is not once the context has ended.
	Started bool
}

// Map calls fn with every item, at most o.Limit at a time, and returns what
// each call came to in the order of the items, whatever order they finished
// in. An item that fails does not stop the others. It runs on Each: once
// ctx ends no further item is started, and those left out come back with
// the context's error. A panic in fn becomes that item's error, with its
// stack in o.PanicLog. Map returns once every call has returned.
//
// The work is reported under the span ctx carries as a step, o.Step, with
// a target for each item, as every pool reports it: every target is
// announced, queued, before the first one runs; each is marked running when
// it takes its place and ended before it gives the place up, so that a
// display never counts more running than the limit; those never started
// end canceled, so that the count reaches its total; and the step ends once
// the last has, naming the items that failed, canceled when every one of
// them ended canceled, as an interrupt leaves a pool. fn is called with the context
// of its item's target, so that the calls it makes are reported under it.
// An item that failed once the context had ended is reported canceled,
// since the interrupt is what ended it, and its outcome keeps the error fn
// returned.
func Map[T, R any](ctx context.Context, items []T, o Options[T], fn func(ctx context.Context, item T) (R, error)) []Outcome[R] {
	limit := o.Limit
	if limit < 1 {
		limit = DefaultMax
	}
	stepCtx, step := progress.Start(ctx, progress.KindStep, o.Step,
		progress.WithFlags(progress.Fold|o.Flags), progress.Total(len(items)), progress.Limit(limit))
	names := make([]string, len(items))
	ctxs := make([]context.Context, len(items))
	spans := make([]*progress.Span, len(items))
	for i, item := range items {
		node, host, role := o.describe(item)
		names[i] = node
		ctxs[i], spans[i] = progress.Start(stepCtx, progress.KindTarget, node, progress.Queued(),
			progress.Node(node), progress.Host(host), progress.Role(role))
	}

	out := make([]Outcome[R], len(items))
	// canceled are the items whose targets ended canceled.
	canceled := make([]bool, len(items))
	Each(ctx, len(items), limit, func(i int) {
		spans[i].Run()
		value, err := call(ctxs[i], o.PanicLog, names[i], items[i], fn)
		out[i] = Outcome[R]{Value: value, Err: err, Started: true}
		if err != nil && ctxs[i].Err() != nil {
			err = ctxs[i].Err()
		}
		canceled[i] = endsCanceled(err)
		spans[i].End(err)
	})
	var failed []string
	var errs []error
	interrupted := true
	for i := range out {
		if !out[i].Started {
			out[i].Err = ctx.Err()
			canceled[i] = endsCanceled(out[i].Err)
			spans[i].End(out[i].Err)
		}
		if out[i].Err != nil {
			failed, errs = append(failed, names[i]), append(errs, out[i].Err)
			interrupted = interrupted && canceled[i]
		}
	}
	step.End(failure("", len(items), failed, errs, interrupted))
	return out
}

// endsCanceled reports whether a target that ends with err ends canceled.
func endsCanceled(err error) bool {
	return err != nil && progress.Classify(err) == progress.ClassCanceled
}

// describe says what a display names an item by.
func (o Options[T]) describe(item T) (node, host, role string) {
	if o.Describe == nil {
		return fmt.Sprint(item), "", ""
	}
	return o.Describe(item)
}

// call calls fn with one item. A panic in it becomes the item's error
// rather than the end of the process, with its stack written to log.
func call[T, R any](ctx context.Context, log io.Writer, name string, item T, fn func(context.Context, T) (R, error)) (value R, err error) {
	defer func() {
		if p := Recovered(log, name, recover()); p != nil {
			var zero R
			value, err = zero, p
		}
	}()
	return fn(ctx, item)
}

// FailureError turns the targets of a fan-out that failed into the error the
// command exits with: "k of n hosts failed: <node set>", with the code
// exitcode.Worst gives their errors, or TargetFailed when none of them says,
// since a command that exited non-zero is a target that failed. The errors
// are kept underneath, not only their text, so that a caller can still tell
// a cancellation from a failure. It is nil when every target succeeded.
func FailureError(results []*transport.Result) error {
	var names []string
	var errs []error
	for _, r := range Failures(results) {
		names, errs = append(names, r.Target.Name), append(errs, r.Err)
	}
	return failure("hosts", len(results), names, errs, false)
}

// failure sums up the items of a fan-out of n that failed: "k of n <noun>
// failed: <node set>", with their names as a node set and each error that
// is not nil underneath. Its progress class is that of its exit code, or
// canceled when the items were interrupted, as their targets ended: the
// error of an item is what its work returned, which does not always say
// that an interrupt stopped it. It is nil when none failed.
func failure(noun string, n int, names []string, errs []error, interrupted bool) error {
	if len(names) == 0 {
		return nil
	}
	set := nodeset.New()
	var kept []error
	for i, name := range names {
		_ = set.Add(name)
		if errs[i] != nil {
			kept = append(kept, errs[i])
		}
	}
	what := "failed"
	if noun != "" {
		what = noun + " failed"
	}
	code := cmp.Or(exitcode.Worst(kept...), exitcode.TargetFailed)
	class := progress.CodeClass(code)
	if interrupted {
		class = progress.ClassCanceled
	}
	return &exitcode.Error{Code: code, Err: &failedTargets{
		message: fmt.Sprintf("%d of %d %s: %s", len(names), n, what, set),
		errs:    kept,
		class:   class,
	}}
}

// failedTargets is the summary of a fan-out that did not succeed
// everywhere, with the error of each target that failed underneath it.
type failedTargets struct {
	message string
	errs    []error
	class   progress.Class
}

func (e *failedTargets) Error() string { return e.message }

func (e *failedTargets) Unwrap() []error { return e.errs }

// ProgressClass says why the fan-out failed as its exit code does, rather
// than as whichever of its targets' errors says a class first.
func (e *failedTargets) ProgressClass() progress.Class { return e.class }
