// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package fanouttest counts the calls a fake is in the middle of, so that a
// test can tell how many of them a fan-out ran at once: the requests a
// transport.Runner is asked to run, those an http.RoundTripper sends to a
// service processor and the connections a host key scan dials.
//
// A fake that answers at once rarely has two calls under way together, so a
// count alone proves little. InFlight holds each call until enough others
// have joined it: a test of a limit holds calls until one more than the
// limit are under way, which a fan-out keeping to its limit never allows,
// so every round is held for the full wait with exactly the limit in
// flight, and a fan-out that starts one call too many is caught with it.
package fanouttest

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// DefaultPatience is how long a call is held when InFlight.Patience is not
// set.
const DefaultPatience = 100 * time.Millisecond

// InFlight counts the calls under way and remembers the most there were at
// once. The zero value counts without holding anything up, and it is safe
// for concurrent use.
type InFlight struct {
	// Hold keeps each call waiting until Hold calls are under way together,
	// or until Patience has passed. Zero holds nothing. Calls that too few
	// others can join, such as the last of a fan-out, wait the whole
	// Patience.
	Hold int
	// Patience is how long a call waits for the others; zero is
	// DefaultPatience.
	Patience time.Duration

	mu      sync.Mutex
	now     int
	peak    int
	started int
	// round is closed when Hold calls are under way together, which lets
	// the calls waiting for it go on.
	round chan struct{}
}

// Enter counts a call in, holds it as Hold says, and returns the function
// that counts it out again. Calling that function more than once counts the
// call out once.
func (f *InFlight) Enter() (leave func()) {
	f.mu.Lock()
	f.now++
	f.started++
	f.peak = max(f.peak, f.now)
	if f.round == nil {
		f.round = make(chan struct{})
	}
	round := f.round
	if f.Hold > 0 && f.now >= f.Hold {
		close(f.round)
		f.round = nil
	}
	f.mu.Unlock()

	if f.Hold > 0 {
		patience := f.Patience
		if patience <= 0 {
			patience = DefaultPatience
		}
		timer := time.NewTimer(patience)
		select {
		case <-round:
		case <-timer.C:
		}
		timer.Stop()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.now--
			f.mu.Unlock()
		})
	}
}

// Peak returns the most calls that were under way at once.
func (f *InFlight) Peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// Started returns how many calls were counted in.
func (f *InFlight) Started() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

// Runner counts the requests r is asked to run, each for as long as it runs.
func (f *InFlight) Runner(r transport.Runner) transport.Runner {
	return runner{f: f, r: r}
}

type runner struct {
	f *InFlight
	r transport.Runner
}

func (c runner) Run(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
	defer c.f.Enter()()
	return c.r.Run(ctx, target, req)
}

// RoundTripper counts the requests rt sends, each until its response has
// arrived.
func (f *InFlight) RoundTripper(rt http.RoundTripper) http.RoundTripper {
	return roundTripper{f: f, rt: rt}
}

type roundTripper struct {
	f  *InFlight
	rt http.RoundTripper
}

func (c roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	defer c.f.Enter()()
	return c.rt.RoundTrip(req)
}

// Dial counts the connections dial makes, each from the dial until it is
// closed, which for a host key scan is the whole scan.
func (f *InFlight) Dial(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		leave := f.Enter()
		conn, err := dial(ctx, network, address)
		if err != nil {
			leave()
			return nil, err
		}
		return &countedConn{Conn: conn, leave: leave}, nil
	}
}

// countedConn counts its connection out when it is closed.
type countedConn struct {
	net.Conn
	leave func()
}

func (c *countedConn) Close() error {
	c.leave()
	return c.Conn.Close()
}
