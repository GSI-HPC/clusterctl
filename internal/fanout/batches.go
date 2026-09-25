// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// BatchOptions say how Batches splits a set and paces its batches.
type BatchOptions struct {
	// Step names the step a display shows the batches under, such as
	// "power on".
	Step string
	// Size is the most nodes a batch holds. The set is split evenly into
	// as few batches as that allows, so ten nodes at 8 go as 5 and 5, not
	// 8 and 2. Below one, the set is one batch.
	Size int
	// Limit is how many targets of a batch are worked on at once, for a
	// display; zero says nothing.
	Limit int
	// Pause is how long to wait between two batches.
	Pause time.Duration
	// After waits out a pause; nil is time.After. The tests replace it.
	After func(time.Duration) <-chan time.Time
	// BeforePause is called before each pause, with its length. There is
	// no pause, and no call, when Pause is zero.
	BeforePause func(pause time.Duration)
	// Before is called before batch i of n is run, counting from 0, once
	// the pause before it is over.
	Before func(i, n int, batch *nodeset.NodeSet)
}

// Batch is one of the batches Batches split a set into, and how it ended.
type Batch struct {
	// Nodes are the nodes of the batch.
	Nodes *nodeset.NodeSet
	// Ran says whether the batch was run.
	Ran bool
	// Err is what running the batch returned. For a batch that was not
	// run it is the context's error when the context had ended by its
	// turn, whether an earlier batch failed or not, and ErrNotTried when
	// an earlier batch failed.
	Err error
}

// ErrNotTried is the error of a batch left out because an earlier one
// failed.
var ErrNotTried = errors.New("not tried: an earlier batch failed")

// Batches runs a set of nodes in batches, one after the other, and returns
// every batch in order with how it ended. A batch is run by calling run,
// which returns once the work for every node of the batch has returned;
// the next is run once o.Pause has passed after that. A batch whose run
// returns an error stops the run: the batches after it are not tried. An
// interrupt stops it too, at the next pause or, without one, before the
// next batch; the batch under way when it came is left to stop by itself,
// as a pool does, and the first batch is always run, so that the work for
// each of its nodes reports how the interrupt found it. The batches an
// interrupt, or the end of ctx, leaves out are left out for that, even
// after one that failed: the failure of a batch the interrupt cut short is
// no reason of its own. Before and
// BeforePause are called on the calling goroutine, for the notes a command
// prints.
//
// The work is reported under the span ctx carries as a step, o.Step, whose
// Total is every node of the set, with a span for each batch, all of them
// queued before the first is run. run is called with the context of its
// batch's span, which is marked running first, and reports the work for
// each node of the batch as a target under it, queued before the first
// runs, as Map does; the batch ends with run's error. Each pause is a
// wait, "stagger". A batch not tried ends skipped, and one the context
// ended before ends canceled, whether it was interrupted or ran out of
// time, and either counts as its Total, so that the step's count reaches
// its own.
func Batches(ctx context.Context, nodes *nodeset.NodeSet, o BatchOptions, run func(ctx context.Context, batch *nodeset.NodeSet) error) []Batch {
	parts := 1
	if n := nodes.Len(); o.Size > 0 && n > o.Size {
		// Rounded up without adding to n, which a Size near the largest
		// int would overflow.
		parts = n / o.Size
		if n%o.Size != 0 {
			parts++
		}
	}
	chunks := nodes.Split(parts)
	after := o.After
	if after == nil {
		after = time.After
	}

	stepCtx, step := progress.Start(ctx, progress.KindStep, o.Step,
		progress.WithFlags(progress.Fold), progress.Total(nodes.Len()))
	out := make([]Batch, len(chunks))
	ctxs := make([]context.Context, len(chunks))
	spans := make([]*progress.Span, len(chunks))
	for i, chunk := range chunks {
		out[i].Nodes = chunk
		ctxs[i], spans[i] = progress.Start(stepCtx, progress.KindBatch, fmt.Sprintf("%d/%d", i+1, len(chunks)),
			progress.Queued(), progress.Batch(i+1, len(chunks)), progress.Node(chunk.String()),
			progress.Total(chunk.Len()), progress.Limit(o.Limit))
	}

	// err is what ended the run: the error of the batch that failed, or
	// the context's, or that of the last batch.
	var err error
	for i, chunk := range chunks {
		if i > 0 {
			if err == nil && o.Pause > 0 {
				pause(ctx, stepCtx, o, after)
			}
			if cause := ctx.Err(); cause != nil {
				for j := i; j < len(chunks); j++ {
					out[j].Err = cause
					spans[j].End(leftOut{cause})
				}
				if err == nil {
					err = cause
				}
				break
			}
			if err != nil {
				for j := i; j < len(chunks); j++ {
					out[j].Err = ErrNotTried
					spans[j].Skip(ErrNotTried.Error())
				}
				break
			}
		}
		if o.Before != nil {
			o.Before(i, len(chunks), chunk)
		}
		spans[i].Run()
		err = run(ctxs[i], chunk)
		out[i].Ran, out[i].Err = true, err
		spans[i].End(err)
	}
	step.End(err)
	return out
}

// leftOut is what a batch the context left out ends with: canceled, since
// it did not fail, whether the context was interrupted or ran out of time,
// so that it counts as its Total.
type leftOut struct{ error }

func (e leftOut) Unwrap() error { return e.error }

// ProgressClass says that the batch was left out, not that it failed.
func (leftOut) ProgressClass() progress.Class { return progress.ClassCanceled }

// pause waits between two batches, until o.Pause has passed or ctx has
// ended, and reports the wait under the step.
func pause(ctx, stepCtx context.Context, o BatchOptions, after func(time.Duration) <-chan time.Time) {
	if o.BeforePause != nil {
		o.BeforePause(o.Pause)
	}
	_, wait := progress.Start(stepCtx, progress.KindWait, "stagger", progress.Timeout(o.Pause))
	select {
	case <-ctx.Done():
		wait.End(ctx.Err())
	case <-after(o.Pause):
		wait.End(nil)
	}
}
