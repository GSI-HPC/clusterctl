// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package fanout works on many items at once, a bounded number at a time,
// and reports each as a target of the progress package: Each is the one
// bounded loop, Map works on items and reports them as a step with a
// target each, and Batches works on a node set in batches, one after the
// other, with a pause between them.
//
// Every pool keeps the promises progresstest.Check holds an emitter to:
// every target is announced queued before the first runs, each is ended
// before its place is given up, and those never started end canceled, so
// that a display's count reaches its total however the work ends. A panic
// in the work for one item becomes that item's error. The package knows no
// program: its name, the rule that tells an error's class and the error a
// step ends with are Options.
package fanout

import (
	"context"
	"sync"
)

// DefaultMax is how many items a pool works on at once when nothing says.
const DefaultMax = 16

// Each calls work with every index below n, at most limit at a time, and
// returns once every call has returned. When ctx ends, no further call is
// started, so an interrupt stops a fan-out the same way wherever it is; the
// caller tells what was left out by what work did not record.
func Each(ctx context.Context, n, limit int, work func(i int)) {
	sem := make(chan struct{}, max(1, min(limit, n)))
	var wg sync.WaitGroup
	for i := range n {
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
			// When a slot and the cancellation are both ready, select
			// picks either, so the context is asked again.
			if ctx.Err() != nil {
				<-sem
			}
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			work(i)
		})
	}
	wg.Wait()
}
