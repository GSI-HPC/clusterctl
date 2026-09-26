// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// step runs a counted step over nodes, each ending with what outcome says
// of it, nil for ok.
func step(ctx context.Context, name string, nodes []string, outcome map[string]error) {
	fanout.Map(ctx, nodes, fanout.Options[string]{Step: name, Limit: 1},
		func(_ context.Context, node string) (struct{}, error) { return struct{}{}, outcome[node] })
}

var three = []string{"exe1", "exe2", "exe3"}

// The summary is left only for a command that ran for a second or more,
// whatever its targets did; it says how the command ended, and counts each
// node once, as the worst of its targets, and a batch left out as its
// Total.
func TestTheSummary(t *testing.T) {
	t.Parallel()
	down := errors.New("connection refused")
	for _, tc := range []struct {
		name string
		work func(ctx context.Context, c *clock) error
		want string
	}{
		{"a command done within a second", func(ctx context.Context, c *clock) error {
			step(ctx, "run", three, nil)
			c.Add(999 * time.Millisecond)
			return nil
		}, ""},
		{"a command that ran for a second", func(ctx context.Context, c *clock) error {
			step(ctx, "run", three, nil)
			c.Add(time.Second)
			return nil
		}, "exec: done in 1.0s: 3 ok"},
		{"a target that failed at once", func(ctx context.Context, c *clock) error {
			step(ctx, "run", three, map[string]error{"exe2": down})
			c.Add(300 * time.Millisecond)
			return errors.New("1 of 3 hosts failed: exe2")
		}, ""},
		{"a target interrupted at once", func(ctx context.Context, c *clock) error {
			step(ctx, "run", three, map[string]error{"exe3": context.Canceled})
			return context.Canceled
		}, ""},
		{"a target that failed in a command that ran for a second", func(ctx context.Context, c *clock) error {
			step(ctx, "run", three, map[string]error{"exe2": down})
			c.Add(1300 * time.Millisecond)
			return errors.New("1 of 3 hosts failed: exe2")
		}, "exec: failed in 1.3s: 2 ok, 1 failed"},
		{"a command that failed at once counting nothing", func(context.Context, *clock) error {
			return errors.New("no such node")
		}, ""},
		{"a command that counted nothing", func(ctx context.Context, c *clock) error {
			_, s := progress.Start(ctx, progress.KindStep, "ping", progress.Total(3))
			c.Add(12 * time.Second)
			s.End(nil)
			return nil
		}, "exec: done in 12s"},
		{"a command that failed counting nothing", func(_ context.Context, c *clock) error {
			c.Add(2 * time.Minute)
			return errors.New("the boot link of exe2 could not be changed")
		}, "exec: failed in 2m00s"},
		{"each node once, as the worst of its targets", func(ctx context.Context, c *clock) error {
			step(ctx, "setting the machines to boot from the network once", three, map[string]error{"exe2": down})
			step(ctx, "clearing the boot overrides", []string{"exe1", "exe3"}, map[string]error{"exe3": context.Canceled})
			c.Add(18*time.Minute + 3*time.Second)
			return down
		}, "exec: failed in 18m03s: 1 ok, 1 failed, 1 canceled"},
		{"a command that failed after its targets all did well", func(ctx context.Context, c *clock) error {
			step(ctx, "reset the machines", three, nil)
			_, s := progress.Start(ctx, progress.KindStep, "forget the host keys")
			c.Add(2 * time.Second)
			err := errors.New("the host key file could not be written")
			s.End(err)
			return err
		}, "exec: failed in 2.0s: 3 ok"},
		{"a hidden step counts for nothing", func(ctx context.Context, c *clock) error {
			hidden, s := progress.Start(ctx, progress.KindStep, "resolve", progress.WithFlags(progress.Hidden))
			step(hidden, "read the groups", three, map[string]error{"exe1": down})
			s.End(nil)
			c.Add(time.Hour + 2*time.Minute + 3*time.Second)
			return nil
		}, "exec: done in 1h02m03s"},
		{"batches left out count as their nodes", func(ctx context.Context, c *clock) error {
			nodes := nodeset.MustParse("exe[1-6]")
			batches := fanout.Batches(ctx, nodes, fanout.BatchOptions{Step: "power on", Size: 2},
				func(ctx context.Context, batch *nodeset.NodeSet) error {
					var err error
					if batch.Contains("exe3") {
						err = down
					}
					step(ctx, "power on", batch.Expand(), map[string]error{"exe3": err})
					return err
				})
			c.Add(time.Second)
			return batches[1].Err
		}, "exec: failed in 1.0s: 3 ok, 1 failed, 2 skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
			summary := &display.Summary{}
			capture := &progresstest.Capture{}
			bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{capture, summary}, Now: c.Now})
			ctx, command := progress.Start(progress.WithBus(context.Background(), bus), progress.KindCommand, "exec")
			err := tc.work(ctx, c)
			if got := summary.Line(); got != "" {
				t.Errorf("a summary before the command ended: %q", got)
			}
			command.End(err)
			bus.Close()
			progresstest.Check(t, capture.Events())
			if got := summary.Line(); got != tc.want {
				t.Errorf("summary %q, want %q", got, tc.want)
			}
		})
	}
}
