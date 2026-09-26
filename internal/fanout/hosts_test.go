// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// Each host is bounded on its own: work on one host waits for a place
// there, and work on another is not held up by it. Every holder waits
// until one more than PerHost are under way on its host, which never
// happens while the bound is kept, so exactly PerHost run on each.
func TestHostsBoundEachHostOnItsOwn(t *testing.T) {
	t.Parallel()

	hosts := &fanout.Hosts{}
	on := map[string]*fanouttest.InFlight{
		"gw1.example.org": {Hold: fanout.PerHost + 1},
		"gw2.example.org": {Hold: fanout.PerHost + 1},
	}
	var wg sync.WaitGroup
	for host, calls := range on {
		for range 10 {
			wg.Go(func() {
				release, err := hosts.Acquire(context.Background(), host)
				if err != nil {
					t.Error(err)
					return
				}
				defer release()
				calls.Enter()()
			})
		}
	}
	wg.Wait()
	for host, calls := range on {
		if got := calls.Peak(); got != fanout.PerHost {
			t.Errorf("%s: %d were under way at once, want %d", host, got, fanout.PerHost)
		}
	}
}

// Work that goes through two hosts takes a place on both, in the same
// order whatever order it names them in, so two holders of one place each
// never wait for each other's.
func TestHostsTakeTheirPlacesInOneOrder(t *testing.T) {
	t.Parallel()

	hosts := &fanout.Hosts{Limit: 1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() {
				pair := []string{"a.example.org", "b.example.org"}
				if i%2 == 1 {
					pair[0], pair[1] = pair[1], pair[0]
				}
				release, err := hosts.Acquire(context.Background(), pair...)
				if err != nil {
					t.Error(err)
					return
				}
				time.Sleep(time.Millisecond)
				release()
				// Giving the places back twice gives them back once.
				release()
			})
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("work through the same two hosts waited for itself")
	}
}

// Once the context ends, a holder waiting for a place stops waiting and
// gives back the places it took, and nothing is taken any more.
func TestHostsGiveBackWhatTheyTookWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	hosts := &fanout.Hosts{Limit: 1}
	holding, err := hosts.Acquire(context.Background(), "b.example.org")
	if err != nil {
		t.Fatal(err)
	}
	defer holding()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := hosts.Acquire(ctx, "a.example.org", "b.example.org"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want the context's error", err)
	}
	quick, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	release, err := hosts.Acquire(quick, "a.example.org")
	if err != nil {
		t.Fatalf("the place on a.example.org was not given back: %v", err)
	}
	release()
	if _, err := hosts.Acquire(ctx, "c.example.org"); !errors.Is(err, context.Canceled) {
		t.Errorf("Acquire after the context ended = %v, want the context's error", err)
	}
}

// An item waits for what Acquire takes with its target queued, since its
// work has not started; one Acquire refuses ends as never started, with
// the reason, and the rest go on. A pool of eight through one host keeps
// to that host's bound.
func TestMapWaitsQueuedForWhatAnItemNeeds(t *testing.T) {
	t.Parallel()

	c, ctx, tree := watchCapture(t)
	hosts := &fanout.Hosts{}
	calls := &fanouttest.InFlight{Hold: fanout.PerHost + 1}
	refused := errors.New("no place for exe2")
	outcomes := fanout.Map(ctx, nodes(8), fanout.Options[string]{
		Step:  "check",
		Limit: 8,
		Acquire: func(ctx context.Context, node string) (func(), error) {
			for _, e := range c.Events() {
				if e.Type == progress.TypeRun && e.Name == node {
					t.Errorf("%s runs before it has its place", node)
				}
			}
			if node == "exe2" {
				return nil, refused
			}
			return hosts.Acquire(ctx, "gw.example.org")
		},
	}, func(context.Context, string) (struct{}, error) {
		calls.Enter()()
		return struct{}{}, nil
	})
	if o := outcomes[1]; o.Started || !errors.Is(o.Err, refused) {
		t.Errorf("the outcome of exe2 is %+v, want it never started, with the refusal", o)
	}
	if got := calls.Peak(); got != fanout.PerHost {
		t.Errorf("%d items were under way at once through one host, want %d", got, fanout.PerHost)
	}
	want := `step check total=8 limit=8 [fold]: failed (target): 1 of 8 failed: exe2
  target exe2: failed (target): no place for {}
  target exe[1,3-8]: ok
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}
