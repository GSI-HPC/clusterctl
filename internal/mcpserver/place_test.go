// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cancelledLate is a context that is not cancelled the first times it is
// asked, and is from then on, the way a cancellation can land between two
// questions a waiting call asks of its context. waiting is closed once the
// call waits on it.
type cancelledLate struct {
	context.Context
	asked, until atomic.Int32
	waiting      chan struct{}
	once         sync.Once
}

func (c *cancelledLate) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return nil
}

func (c *cancelledLate) Err() error {
	if c.asked.Add(1) > c.until.Load() {
		return context.Canceled
	}
	return nil
}

// A call that took a place as it was cancelled either keeps the place, and
// gives it back when it ends, or is turned away and gives it back at once,
// however the cancellation falls between the questions it asks: a place
// kept by a call that is gone would be gone for good, and with it half the
// server.
func TestAPlaceIsNeverKeptByACallTurnedAway(t *testing.T) {
	for until := int32(0); until < 4; until++ {
		s := &Server{opts: Options{Log: io.Discard}, calls: make(chan struct{}, maxCalls)}
		for range maxCalls {
			s.calls <- struct{}{}
		}
		ctx := &cancelledLate{Context: context.Background(), waiting: make(chan struct{})}
		ctx.until.Store(until)
		handler := limited(s, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
		done := make(chan error)
		go func() {
			_, _, err := handler(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "describe_nodes"}}, struct{}{})
			done <- err
		}()
		// The waiting call takes the place another call gives back.
		<-ctx.waiting
		<-s.calls
		err := <-done
		if n := len(s.calls); n != maxCalls-1 {
			t.Errorf("cancelled after %d questions (err %v): %d places taken once the call was done, want %d",
				until, err, n, maxCalls-1)
		}
	}
}
