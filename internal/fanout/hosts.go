// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout

import (
	"context"
	"slices"
	"sync"
)

// PerHost is how many sessions a command opens to one infrastructure host
// at once (ADR 0022). By default sshd starts to drop connections once ten
// are waiting to authenticate, and a role that multiplexes its connections
// carries at most ten sessions on one; four leaves room for the other
// administrators and for the jobs of the host itself.
const PerHost = 4

// Hosts bounds the work under way on each host: at most Limit at a time on
// any one of them, however many hosts a pool works on at once. The zero
// value bounds each host to PerHost, and a Hosts is safe for concurrent
// use; one Hosts shared by pools side by side bounds them together.
type Hosts struct {
	// Limit is how many may be under way on one host; below one is
	// PerHost.
	Limit int

	mu    sync.Mutex
	slots map[string]chan struct{}
}

// Acquire takes a place on every host it is given, waiting while one of
// them is full, and returns the function that gives them back, which may
// be called more than once. The places are taken in the order of the
// hosts' names, whatever order they are given in, so two callers that need
// the same two hosts cannot each hold one and wait for the other; a host
// given twice, or an empty name, takes no second place. Once ctx has ended
// nothing is taken: what was is given back, and the context's error
// returned.
func (h *Hosts) Acquire(ctx context.Context, hosts ...string) (release func(), err error) {
	names := slices.Compact(slices.Sorted(slices.Values(hosts)))
	var held []chan struct{}
	var once sync.Once
	release = func() {
		once.Do(func() {
			for _, slot := range held {
				<-slot
			}
		})
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		slot := h.slot(name)
		select {
		case slot <- struct{}{}:
			held = append(held, slot)
		case <-ctx.Done():
		}
		// When a place and the cancellation are both ready, select picks
		// either, so the context is asked again.
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
	}
	return release, nil
}

// slot returns the places of one host.
func (h *Hosts) slot(host string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.slots == nil {
		h.slots = map[string]chan struct{}{}
	}
	slot, ok := h.slots[host]
	if !ok {
		limit := h.Limit
		if limit < 1 {
			limit = PerHost
		}
		slot = make(chan struct{}, limit)
		h.slots[host] = slot
	}
	return slot
}
