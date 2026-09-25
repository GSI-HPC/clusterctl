// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanouttest_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// pool runs calls calls, width of them at once, the way a fan-out bounded
// at width does.
func pool(width, calls int, call func()) {
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	for range calls {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			call()
		})
	}
	wg.Wait()
}

// Holding calls until one more than the limit are under way is what makes a
// count worth something: a pool that keeps to its limit is seen with exactly
// that many in flight, and one that runs a call too many is caught with it.
func TestInFlightTellsALimitKeptFromOneExceeded(t *testing.T) {
	t.Parallel()
	const limit = 2
	tests := []struct {
		name  string
		width int
	}{
		{"a pool that keeps to the limit", limit},
		{"a pool that runs one call too many", limit + 1},
		{"a pool that runs one call at a time", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := &fanouttest.InFlight{Hold: limit + 1, Patience: 20 * time.Millisecond}
			pool(tc.width, 6, func() { calls.Enter()() })
			if got := calls.Peak(); got != tc.width {
				t.Errorf("Peak = %d, want %d", got, tc.width)
			}
			if got := calls.Started(); got != 6 {
				t.Errorf("Started = %d, want 6", got)
			}
		})
	}
}

// Calls that are all under way together are let go at once rather than
// after the wait.
func TestInFlightLetsAFullRoundGoAtOnce(t *testing.T) {
	t.Parallel()
	calls := &fanouttest.InFlight{Hold: 4, Patience: time.Minute}
	start := time.Now()
	pool(4, 4, func() { calls.Enter()() })
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("a full round took %s; it waited for the patience to run out", elapsed)
	}
	if got := calls.Peak(); got != 4 {
		t.Errorf("Peak = %d, want 4", got)
	}
}

// Each adapter counts its call for as long as it is under way: a request
// until it is answered, a connection until it is closed, however often.
func TestInFlightCountsEachKindOfCall(t *testing.T) {
	t.Parallel()
	t.Run("ssh requests", func(t *testing.T) {
		t.Parallel()
		calls := &fanouttest.InFlight{Hold: 3, Patience: time.Minute}
		runner := calls.Runner(&transport.Recorder{})
		pool(3, 3, func() {
			if _, err := runner.Run(context.Background(), transport.Target{Name: "exe1"}, transport.Request{Argv: []string{"true"}}); err != nil {
				t.Error(err)
			}
		})
		if got := calls.Peak(); got != 3 {
			t.Errorf("Peak = %d, want 3", got)
		}
	})

	t.Run("Redfish requests", func(t *testing.T) {
		t.Parallel()
		calls := &fanouttest.InFlight{Hold: 3, Patience: time.Minute}
		client := &http.Client{Transport: calls.RoundTripper(answer(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
		}))}
		pool(3, 3, func() {
			resp, err := client.Get("https://exe0001.mgmt.example.org/redfish/v1/")
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
		})
		if got := calls.Peak(); got != 3 {
			t.Errorf("Peak = %d, want 3", got)
		}
	})

	t.Run("host key connections", func(t *testing.T) {
		t.Parallel()
		calls := &fanouttest.InFlight{}
		dial := calls.Dial(func(context.Context, string, string) (net.Conn, error) {
			conn, peer := net.Pipe()
			t.Cleanup(func() { _ = peer.Close() })
			return conn, nil
		})
		open := func() net.Conn {
			conn, err := dial(context.Background(), "tcp", "exe0001.example.org:22")
			if err != nil {
				t.Fatal(err)
			}
			return conn
		}
		first, second := open(), open()
		// A scan closes its connection when its context ends and again
		// when it returns; the second close must not count it out again.
		_ = first.Close()
		_ = first.Close()
		third := open()
		if got := calls.Peak(); got != 2 {
			t.Errorf("Peak = %d, want 2: a closed connection is not under way", got)
		}
		fourth := open()
		if got := calls.Peak(); got != 3 {
			t.Errorf("Peak = %d, want 3 with three connections open", got)
		}
		for _, conn := range []net.Conn{second, third, fourth} {
			_ = conn.Close()
		}
	})
}

// answer is a RoundTripper made of a function.
type answer func(*http.Request) (*http.Response, error)

func (a answer) RoundTrip(req *http.Request) (*http.Response, error) { return a(req) }
