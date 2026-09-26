// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// lockedBuffer is a buffer the notifier's goroutine and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A notification that panics as it is sent stops the notifications of its
// call, with the stack in the server's log, and ends neither the call nor
// the server: the server holds every plan not yet applied.
func TestANotifierThatPanicsStopsAndSaysWhy(t *testing.T) {
	var log lockedBuffer
	sent := 0
	n := newNotifier(func(*mcp.ProgressNotificationParams) {
		sent++
		panic("the session is gone")
	}, &log)
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{n}})
	n.start()
	ctx := progress.WithBus(context.Background(), bus)
	stepCtx, step := progress.Start(ctx, progress.KindStep, "read the groups", progress.WithFlags(progress.Fold), progress.Total(2))
	for _, node := range []string{"exe1", "exe2"} {
		_, target := progress.Start(stepCtx, progress.KindTarget, node, progress.Queued(), progress.Node(node))
		target.Run()
		target.End(nil)
	}
	step.End(nil)
	bus.Close()
	n.flush()
	if !strings.Contains(log.String(), "progress notifications stopped: the session is gone") || !strings.Contains(log.String(), "goroutine") {
		t.Errorf("the log says %q, want the panic and its stack", log.String())
	}
	if sent != 1 {
		t.Errorf("%d notifications were tried, want the one that panicked alone", sent)
	}
}
