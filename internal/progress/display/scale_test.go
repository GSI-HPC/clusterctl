// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
)

// fanOutCost is the best of five runs of a step over n targets, each
// started queued, run and ended in turn, with a frame drawn every
// frameEvery targets, through a tree on a terminal of 100 by 30.
func fanOutCost(t *testing.T, n, frameEvery int, end func(i int) error) time.Duration {
	t.Helper()
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("exe%05d", i+1)
	}
	best := time.Duration(1 << 62)
	for range 5 {
		screen := &progresstest.Screen{Width: 100}
		term := display.NewTerminal(screen, func() (int, int, error) { return 100, 30, nil })
		c := &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
		tree := display.NewTree(term, display.TreeOptions{Now: c.Now})
		bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{tree}, Now: c.Now})
		runtime.GC()
		start := time.Now()
		ctx, command := progress.Start(progress.WithBus(context.Background(), bus), progress.KindCommand, "exec")
		ctx, step := progress.Start(ctx, progress.KindStep, "run", progress.WithFlags(progress.Fold),
			progress.Total(n), progress.Limit(n))
		spans := make([]*progress.Span, n)
		for i, name := range names {
			_, spans[i] = progress.Start(ctx, progress.KindTarget, name, progress.Queued(), progress.Node(name))
		}
		for i, span := range spans {
			span.Run()
			if frameEvery > 0 && i%frameEvery == 0 {
				c.Add(100 * time.Millisecond)
				tree.Draw()
			}
		}
		for i, span := range spans {
			span.End(end(i))
			if frameEvery > 0 && i%frameEvery == 0 {
				c.Add(100 * time.Millisecond)
				tree.Draw()
			}
		}
		step.End(nil)
		command.End(nil)
		best = min(best, time.Since(start))
		bus.Close()
		tree.Close()
	}
	return best
}

// The tree took each target that ended out of the list of its step's
// targets with a pass over the others: a step of 20,000 targets took more
// than half a second over its events, with the Bus waiting, where 2,000
// took 17ms. Thirty times as many targets have to cost about thirty times
// as much.
func TestEndingTheTargetsOfAStepCostsWhatTheyNumber(t *testing.T) {
	if raceDetector {
		t.Skip("the costs are measured without the race detector")
	}
	ok := func(int) error { return nil }
	small, large := fanOutCost(t, 1000, 0, ok), fanOutCost(t, 30000, 0, ok)
	if large > 90*small {
		t.Errorf("30,000 targets took %v, %d times what 1,000 took; it should be about 30", large, large/max(small, 1))
	}
}
