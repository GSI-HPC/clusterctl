// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"context"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/mcpprogress"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

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
	var n *mcpprogress.Notifier
	var sinks []progress.Sink
	if req != nil && req.Params != nil && req.Session != nil {
		if token := req.Params.GetProgressToken(); token != nil {
			session := req.Session
			n = mcpprogress.New(mcpprogress.Options{
				// A notification that cannot be sent stops the
				// notifications of the call and changes nothing about it:
				// the client has gone, or given up on it.
				Send: func(ctx context.Context, progress, total float64, message string) error {
					return session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
						ProgressToken: token, Progress: progress, Total: total, Message: message,
					})
				},
				PanicLog: s.opts.Log,
				Program:  "clusterctl",
			})
			sinks = append(sinks, n)
		}
	}
	bus := progress.NewBus(progress.BusOptions{Sinks: sinks, PanicLog: s.opts.Log, Program: "clusterctl", Classify: exitcode.Class})
	return progress.WithBus(ctx, bus), func() {
		bus.Close()
		if n != nil {
			// Only the end of the call's context makes Close fail, and
			// then the call has no one left to tell.
			_ = n.Close(ctx)
		}
	}
}

// trace names the trace of the call ctx belongs to, for its audit entries.
func trace(ctx context.Context) string {
	if bus := progress.BusFrom(ctx); bus != nil {
		return bus.TraceID().String()
	}
	return ""
}
