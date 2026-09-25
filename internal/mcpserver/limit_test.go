// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
)

// probe is the argument list that runs the read command of tree.
var probe = map[string]any{"args": []string{"probe"}}

// callAtOnce runs n calls of read_command at once and fails the test for
// each that fails.
func (f *fixture) callAtOnce(t *testing.T, n int, args map[string]any) {
	t.Helper()
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			res, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_command", Arguments: args})
			if err != nil {
				t.Errorf("read_command: %v", err)
			} else if res.IsError {
				t.Errorf("read_command failed: %s", text(res))
			}
		})
	}
	wg.Wait()
}

// The SDK starts every call as soon as it arrives, and each call can fan out
// as wide as fanout.max, so calls sent side by side multiplied it. Each call
// is held here until one more than the limit are under way, which never
// happens while the limit is kept.
func TestTheServerWorksOnAtMostTwoCallsAtOnce(t *testing.T) {
	calls := &fanouttest.InFlight{Hold: 3}
	f := start(t, setup{command: tree(func(*cobra.Command) error {
		calls.Enter()()
		return nil
	})})

	f.callAtOnce(t, 5, probe)
	if got := calls.Peak(); got != 2 {
		t.Errorf("%d calls were worked on at once, want 2", got)
	}
	if got := calls.Started(); got != 5 {
		t.Errorf("%d calls ran, want 5", got)
	}
}

// A call waiting for the administrator's answer holds no place: otherwise
// two questions left open would stop every other call until they were
// answered. The answer comes on a retry of the call, or, for a client on an
// older protocol, the SDK asks and runs the handler again; neither holds a
// place while the person decides.
func TestACallWaitingForTheUserHoldsNoPlace(t *testing.T) {
	for _, protocol := range []string{"", "2025-11-25"} {
		t.Run("protocol "+protocol, func(t *testing.T) {
			asked, answer := make(chan struct{}), make(chan struct{})
			calls := &fanouttest.InFlight{Hold: 2, Patience: 5 * time.Second}
			f := start(t, setup{
				protocol: protocol,
				answer: func(*mcp.ElicitRequest) *mcp.ElicitResult {
					close(asked)
					<-answer
					return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}
				},
				command: tree(func(*cobra.Command) error {
					calls.Enter()()
					return nil
				}),
			})
			p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})

			applied := make(chan *mcp.CallToolResult, 1)
			go func() {
				res, err := f.session.CallTool(context.Background(),
					&mcp.CallToolParams{Name: "apply_plan", Arguments: applyArgs(p)})
				if err != nil {
					res = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
				}
				applied <- res
			}()
			select {
			case <-asked:
			case res := <-applied:
				t.Fatalf("apply_plan returned without asking: %s", text(res))
			}
			start := time.Now()
			f.callAtOnce(t, 2, probe)
			if got := calls.Peak(); got != 2 {
				t.Errorf("%d calls ran at once while the question was open, want 2", got)
			}
			if elapsed := time.Since(start); elapsed > 4*time.Second {
				t.Errorf("two calls took %s while the question was open", elapsed)
			}

			close(answer)
			if res := <-applied; res.IsError {
				t.Errorf("apply_plan failed once the user answered: %s", text(res))
			}
			if sent := f.cluster.sent(); len(sent) != 1 {
				t.Errorf("sent %v, want the resume", sent)
			}
		})
	}
}

// A call the client gave up on while it waited for a place is not run once
// a place comes free.
func TestACallCancelledWhileItWaitsIsNotRun(t *testing.T) {
	var (
		entered   = make(chan struct{}, 2)
		release   = make(chan struct{})
		ranLate   atomic.Bool
		cancelled = map[string]any{"args": []string{"probe", "cancelled"}}
	)
	log := &lockedBuffer{}
	f := start(t, setup{log: log, command: tree(func(cmd *cobra.Command) error {
		if cmd.Flags().Arg(0) == "cancelled" {
			ranLate.Store(true)
			return nil
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	})})

	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free()
	both := make(chan struct{})
	go func() {
		defer close(both)
		f.callAtOnce(t, 2, probe)
	}()
	<-entered
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.session.CallTool(ctx, &mcp.CallToolParams{Name: "read_command", Arguments: cancelled}); err == nil {
		t.Error("a call that waited past its deadline returned without an error")
	}
	// The client tells the server it gave up on the call after the fact;
	// the places come free only once the server has heard.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "read_command was given up on while it waited") {
		if time.Now().After(deadline) {
			t.Fatalf("the server did not give up on the call:\n%s", log)
		}
		time.Sleep(10 * time.Millisecond)
	}

	free()
	<-both
	// A place is free again, and the call given up on would have taken it
	// before this one got it.
	f.callAtOnce(t, 1, probe)
	if ranLate.Load() {
		t.Error("the call the client gave up on was run once a place came free")
	}
}
