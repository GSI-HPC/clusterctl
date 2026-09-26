// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// notifyEvery is how often a call's progress is sent at most, but as a
// step ends.
const notifyEvery = 500 * time.Millisecond

// watch gives a tool call a progress Bus of its own, which everything the
// call reports goes to, and returns what closes it once the call is over.
// When the client asked for the call's progress, with a progress token, a
// notifier sends it; closing sends what is left, so that no notification
// follows the call's result. A Bus the context brought, as a test's, is
// used as it is, and nothing is sent.
func (s *Server) watch(ctx context.Context, req *mcp.CallToolRequest) (context.Context, func()) {
	if progress.BusFrom(ctx) != nil {
		return ctx, func() {}
	}
	var n *notifier
	var sinks []progress.Sink
	if req != nil && req.Params != nil && req.Session != nil {
		if token := req.Params.GetProgressToken(); token != nil {
			session := req.Session
			n = newNotifier(func(p *mcp.ProgressNotificationParams) {
				p.ProgressToken = token
				// A notification that cannot be sent changes nothing about
				// the call: the client has gone, or given up on it.
				_ = session.NotifyProgress(ctx, p)
			}, s.opts.Log)
			sinks = append(sinks, n)
		}
	}
	bus := progress.NewBus(progress.Options{Sinks: sinks, PanicLog: s.opts.Log})
	if n != nil {
		n.start()
	}
	return progress.WithBus(ctx, bus), func() {
		bus.Close()
		if n != nil {
			n.flush()
		}
	}
}

// trace names the trace of the call ctx belongs to, for its audit entries.
func trace(ctx context.Context) string {
	if bus := progress.BusFrom(ctx); bus != nil {
		return bus.Trace().String()
	}
	return ""
}

// notifier sends the progress of one tool call to the client, as MCP
// progress notifications: how many of the targets the call expects have
// ended, of how many, and a line for a person to read, such as "read the
// groups: 3/16 done, 1 failed". It sends at most every half second, and as
// each step ends, from a goroutine of its own, never while the Bus is
// locked.
//
// What is expected is the Total of every step that counts its targets,
// with none above it that does, a root of a progress.Tally, that the call
// has started; it may grow as another starts, but never shrinks. What has
// ended is the targets ended below the roots under way, and the whole
// Total of each root that has ended, so it never goes back, never passes
// what is expected, and, once every root has ended, is all of it. A call
// that counts nothing sends nothing. Hidden steps are not counted, as a
// display does not show them.
//
// MCP asks that the progress of each notification be more than the one
// before: after the first, a notification is sent only once more targets
// have ended, and says what is expected and where the call stands by
// then. Its line is about the root that started last, and, when the
// numbers count more than that root, ends with what they count in all:
// "read the uptime: 0/6 done; 6/12 in all".
//
// A notifier is a progress.Sink; its methods are safe for concurrent use.
type notifier struct {
	send func(*mcp.ProgressNotificationParams)
	// log receives the stack of a panic while a notification was made or
	// sent, the server's log; the notifier sends nothing more after one,
	// and the call and the server go on.
	log io.Writer
	// broken says the notifier panicked.
	broken atomic.Bool

	mu    sync.Mutex
	tally progress.Tally
	// roots are the roots of the tally that are shown and open.
	roots map[progress.SpanID]bool
	// ended is the Total of the roots that have ended, and last says how
	// the one that ended last stood; counted is how many roots have
	// started, ended or not.
	ended, counted int
	last           string
	// dirty says something changed since the last notification, and
	// urgent that a step ended.
	dirty, urgent bool
	// sent is what the last notification said, and sentAt when; any says
	// one was sent.
	sent   progressState
	sentAt time.Time
	any    bool

	wake          chan struct{}
	stop, stopped chan struct{}
}

// progressState is what a notification says.
type progressState struct {
	progress, total int
	message         string
}

func newNotifier(send func(*mcp.ProgressNotificationParams), log io.Writer) *notifier {
	return &notifier{
		send:    send,
		log:     log,
		roots:   map[progress.SpanID]bool{},
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Handle counts e in, and has a notification sent if it changed anything.
func (n *notifier) Handle(e progress.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	count, counted := n.tally.Add(e)
	shown := e.Flags&progress.Hidden == 0
	switch e.Type {
	case progress.TypeStart:
		if _, ok := n.tally.Count(e.Span); ok && shown {
			for _, root := range n.tally.Roots() {
				if root.Span == e.Span {
					n.roots[e.Span] = true
					n.counted++
				}
			}
		}
	case progress.TypeEnd:
		if counted && n.roots[e.Span] {
			delete(n.roots, e.Span)
			n.ended += count.Total
			n.last = standing(count)
		}
		if shown && (e.Kind == progress.KindStep || e.Kind == progress.KindBatch) {
			n.urgent = true
		}
	}
	n.dirty = true
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// start sends the notifications as they fall due, until flush.
func (n *notifier) start() {
	go func() {
		defer close(n.stopped)
		defer n.recovered()
		var later <-chan time.Time
		for {
			select {
			case <-n.stop:
				return
			case <-n.wake:
			case <-later:
				later = nil
			}
			n.mu.Lock()
			if wait := time.Until(n.sentAt.Add(notifyEvery)); n.dirty && !n.urgent && wait > 0 {
				if later == nil {
					later = time.After(wait)
				}
				n.mu.Unlock()
				continue
			}
			p := n.due()
			n.mu.Unlock()
			if p != nil {
				n.send(p)
			}
		}
	}()
}

// flush stops the sending, and sends where the call stands now if that is
// not what was sent last. Nothing is sent once it has returned.
func (n *notifier) flush() {
	close(n.stop)
	<-n.stopped
	if n.broken.Load() {
		return
	}
	defer n.recovered()
	n.mu.Lock()
	p := n.due()
	n.mu.Unlock()
	if p != nil {
		n.send(p)
	}
}

// recovered, deferred where the notifier makes and sends notifications,
// keeps a panic there from ending the server, and every plan it holds: the
// stack goes to the server's log, and nothing more is sent for the call.
func (n *notifier) recovered() {
	p := recover()
	if p == nil {
		return
	}
	n.broken.Store(true)
	// The log is a courtesy; a write that fails changes nothing.
	_, _ = fmt.Fprintf(n.log, "clusterctl mcp: progress notifications stopped: %v\n%s", p, debug.Stack())
}

// due returns the notification to send, nil when nothing is counted or no
// more targets have ended since the last one. n.mu is held.
func (n *notifier) due() *mcp.ProgressNotificationParams {
	if !n.dirty {
		return nil
	}
	n.dirty, n.urgent = false, false
	now := progressState{progress: n.ended, total: n.ended, message: n.last}
	for _, root := range n.tally.Roots() {
		if n.roots[root.Span] {
			now.progress += min(root.Done, root.Total)
			now.total += root.Total
			now.message = standing(root)
		}
	}
	if now.total == 0 || n.any && now.progress <= n.sent.progress {
		return nil
	}
	if n.counted > 1 {
		now.message += fmt.Sprintf("; %d/%d in all", now.progress, now.total)
	}
	n.sent, n.sentAt, n.any = now, time.Now(), true
	return &mcp.ProgressNotificationParams{
		Progress: float64(now.progress),
		Total:    float64(now.total),
		Message:  now.message,
	}
}

// standing says how far a root has got: "read the groups: 3/16 done, 1
// failed".
func standing(c progress.Count) string {
	parts := []string{fmt.Sprintf("%d/%d done", c.Done, c.Total)}
	if c.Batch != "" {
		parts = append([]string{"batch " + c.Batch}, parts...)
	}
	for _, k := range []struct {
		n    int
		what string
	}{{c.Failed, "failed"}, {c.Canceled, "canceled"}, {c.Skipped, "skipped"}} {
		if k.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", k.n, k.what))
		}
	}
	return c.Name + ": " + strings.Join(parts, ", ")
}
