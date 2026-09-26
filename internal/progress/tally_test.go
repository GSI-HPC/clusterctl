// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// tallied feeds a Tally the way a display does, and keeps what it said of
// every span that counts when that span ended.
type tallied struct {
	tally progress.Tally
	ended map[string]progress.Count
}

func (s *tallied) Handle(e progress.Event) {
	if c, ok := s.tally.Add(e); ok {
		s.ended[c.Name] = c
	}
}

// A target counts once in every step and batch above it, however it
// ended, and a batch left out counts as its Total; only the outermost of
// the spans that count is a root.
func TestTallyCountsEveryTargetOnce(t *testing.T) {
	t.Parallel()
	sink := &tallied{ended: map[string]progress.Count{}}
	ctx, _, _ := watched(t, progress.Options{Sinks: []progress.Sink{sink}})
	ctx, cmd := progress.Start(ctx, progress.KindCommand, "bmc power on")

	targets := func(ctx context.Context, n int) []*progress.Span {
		spans := make([]*progress.Span, n)
		for i := range spans {
			node := fmt.Sprintf("exe%d", i+1)
			_, spans[i] = progress.Start(ctx, progress.KindTarget, node, progress.Queued(), progress.Node(node))
		}
		return spans
	}

	// A fan-out: one target ends well, one fails, one is interrupted
	// before it starts.
	stepCtx, step := progress.Start(ctx, progress.KindStep, "reset the machines",
		progress.WithFlags(progress.Fold), progress.Total(3), progress.Limit(1))
	fan := targets(stepCtx, 3)
	fan[0].Run()
	fan[0].End(nil)
	fan[1].Run()
	fan[1].End(errors.New("no answer"))
	if roots := sink.tally.Roots(); len(roots) != 1 || roots[0].Name != "reset the machines" ||
		roots[0].Done != 2 || roots[0].Failed != 1 || roots[0].Queued != 1 || roots[0].Running != 0 {
		t.Errorf("roots while the fan-out runs = %+v", roots)
	}
	fan[2].End(context.Canceled)
	step.End(errors.New("2 of 3 failed"))

	// Batches: the first runs, and the second is left out after it.
	stepCtx, step = progress.Start(ctx, progress.KindStep, "power on", progress.WithFlags(progress.Fold), progress.Total(4))
	first, one := progress.Start(stepCtx, progress.KindBatch, "1/2", progress.Queued(), progress.Batch(1, 2), progress.Total(2))
	_, two := progress.Start(stepCtx, progress.KindBatch, "2/2", progress.Queued(), progress.Batch(2, 2), progress.Total(2))
	if roots := sink.tally.Roots(); len(roots) != 1 || roots[0].Queued != 4 || roots[0].Batch != "" {
		t.Errorf("roots before the first batch = %+v", roots)
	}
	one.Run()
	for _, target := range targets(first, 2) {
		target.Run()
		target.End(nil)
	}
	if roots := sink.tally.Roots(); len(roots) != 1 || roots[0].Done != 2 || roots[0].Queued != 2 || roots[0].Batch != "1/2" {
		t.Errorf("roots after the first batch = %+v", roots)
	}
	one.End(nil)
	two.Skip("not tried: an earlier batch failed")
	step.End(nil)
	cmd.End(nil)

	for _, want := range []progress.Count{
		{Name: "reset the machines", Flags: progress.Fold, Total: 3, Done: 3, Failed: 1, Canceled: 1, Targets: 3},
		{Name: "power on", Flags: progress.Fold, Total: 4, Done: 4, Skipped: 2, Targets: 2, Batch: "1/2"},
		{Name: "1/2", Total: 2, Done: 2, Targets: 2, Batch: "1/2"},
		{Name: "2/2", Total: 2, Batch: "2/2"},
	} {
		got := sink.ended[want.Name]
		got.Span = 0
		if got != want {
			t.Errorf("%s ended as\n%+v\nwant\n%+v", want.Name, got, want)
		}
	}
	if roots := sink.tally.Roots(); len(roots) != 0 {
		t.Errorf("roots once everything ended = %+v", roots)
	}
}
