// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package mcpserver offers clusterctl to an AI agent over the Model Context
// Protocol.
//
// The agent is given a few tools shaped around what it is asked to find out,
// rather than one tool per command: resolve a node set, describe nodes across
// the inventory and the workload manager, query Slurm, and run any command
// the command tree marks as read-only. Nothing changes the site except
// through a plan: plan_change resolves and previews an action exactly as the
// confirmation gate would, and apply_plan carries it out once the
// administrator, not the agent, has answered the gate's question.
//
// The server runs on the administrator's workstation, over stdio, as the
// administrator: it reaches the site with their ssh agent and their
// configuration, and it is pinned to the context it was started in.
package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// ConfirmMode says who answers the confirmation gate before a plan is
// applied.
type ConfirmMode string

const (
	// ConfirmElicit asks the administrator through the client, with an MCP
	// elicitation the agent cannot answer. A client that cannot ask is
	// refused.
	ConfirmElicit ConfirmMode = "elicit"
	// ConfirmApproval leaves the question to the client's own approval of
	// the apply_plan call, which shows the node set and the count the agent
	// had to repeat. It is only as good as the client's approval settings.
	ConfirmApproval ConfirmMode = "approval"
)

// ConfirmModes lists the accepted modes.
func ConfirmModes() []string {
	return []string{string(ConfirmElicit), string(ConfirmApproval)}
}

// Options configure a server.
type Options struct {
	// App holds the global options every call resolves the configuration
	// with. Its context is pinned when the server starts; the node set,
	// dry run, confirmation and force options are ignored.
	App app.Options
	// StateDir and CacheDir are where the calls keep their state and cache.
	StateDir string
	CacheDir string
	// Command builds a fresh command tree writing to the given streams. The
	// read_command tool runs the read-only commands through it.
	Command func(ctx context.Context, streams app.Streams) *cobra.Command
	// Confirm says who answers the confirmation gate.
	Confirm ConfirmMode
	// PlanTTL is how long a plan may wait to be applied.
	PlanTTL time.Duration
	// Version is reported to the client.
	Version string
	// Log receives the server's own messages. Standard output carries the
	// protocol, so this is never it.
	Log io.Writer
	// Now replaces the clock in tests.
	Now func() time.Time
}

// maxCalls is how many tool calls the server works on at once. The SDK
// starts every call as soon as it arrives, and each can reach as many hosts
// at once as fanout.max allows, so an agent sending calls side by side would
// multiply the connections one command is allowed.
const maxCalls = 2

// Server is a clusterctl MCP server.
type Server struct {
	opts    Options
	context string
	cluster string
	plans   *planStore
	audit   *auditLog
	sdk     *mcp.Server
	// calls holds a place for each tool call being worked on.
	calls chan struct{}
}

// New resolves the configuration once, pins the context it names and builds
// the server.
func New(ctx context.Context, opts Options) (*Server, error) {
	opts.Confirm = cmp.Or(opts.Confirm, ConfirmElicit)
	switch opts.Confirm {
	case ConfirmElicit, ConfirmApproval:
	default:
		return nil, exitcode.Errorf(exitcode.Usage, "unknown confirmation mode %q; use %s",
			opts.Confirm, strings.Join(ConfirmModes(), " or "))
	}
	if opts.PlanTTL <= 0 {
		opts.PlanTTL = 10 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Command == nil {
		return nil, errors.New("mcpserver: no command tree was given")
	}

	s := &Server{opts: opts, calls: make(chan struct{}, maxCalls)}
	a, err := s.app(ctx)
	if err != nil {
		return nil, err
	}
	// A context that changed under a running server would move the agent to
	// another cluster between two calls, so the one it started in is kept.
	s.context = a.Resolved.Context.Name
	s.cluster = a.Resolved.ClusterName
	s.plans = newPlanStore(opts.PlanTTL, opts.Now)
	s.audit = &auditLog{path: filepath.Join(opts.StateDir, "mcp", "audit.jsonl"), now: opts.Now}

	s.sdk = mcp.NewServer(&mcp.Implementation{
		Name:    "clusterctl",
		Title:   "clusterctl",
		Version: opts.Version,
	}, &mcp.ServerOptions{
		Instructions: s.instructions(),
		Capabilities: &mcp.ServerCapabilities{},
	})
	s.sdk.AddReceivingMiddleware(s.recoverPanics)
	s.addReadTools()
	s.addPlanTools()
	s.addCommandTool()
	return s, nil
}

// Run serves one client over a transport until it disconnects or the context
// ends.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	s.logf("serving context %s (cluster %s), confirmation by %s", s.context, s.cluster, s.opts.Confirm)
	return s.sdk.Run(ctx, t)
}

// SDK returns the underlying server, for tests that connect a client in
// memory.
func (s *Server) SDK() *mcp.Server { return s.sdk }

// app resolves the command context for one call. Every call reads the
// configuration afresh, so an edit to the inventory or the protected hosts
// is seen without restarting, and every call gets its own streams, so that
// nothing a command prints can reach the protocol on standard output.
func (s *Server) app(ctx context.Context) (*app.App, error) {
	opts := s.opts.App
	if s.context != "" {
		opts.Context = s.context
	}
	opts.Format = "json"
	// No default node set: a call names its nodes or selects nothing.
	opts.Nodes = ""
	opts.DryRun = false
	opts.AssumeYes = false
	opts.Force = false
	return app.New(ctx, s.streams(io.Discard), opts)
}

// streams are what a call reads and writes. There is no terminal: a command
// that would prompt refuses instead. What a password helper says on its
// standard error, and the stack of a panic, go to the server's log rather
// than into the notes the agent reads.
func (s *Server) streams(out io.Writer) app.Streams {
	return app.Streams{
		In:       strings.NewReader(""),
		Out:      out,
		Err:      out,
		Diag:     s.opts.Log,
		IsTTY:    false,
		StateDir: s.opts.StateDir,
		CacheDir: s.opts.CacheDir,
	}
}

func (s *Server) logf(format string, args ...any) {
	// The log is a courtesy; a write that fails changes nothing.
	_, _ = fmt.Fprintf(s.opts.Log, "clusterctl mcp: "+format+"\n", args...)
}

// recoverPanics turns a panic in a handler into a failed call. The server
// holds every plan waiting to be applied, and one bad call must not end it
// and them with it.
func (s *Server) recoverPanics(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			panicked := fmt.Sprint(v)
			s.logf("%s panicked: %q\n%s", method, panicked, debug.Stack())
			failure := fmt.Errorf("failed: clusterctl panicked; this is a bug, please report it: %q", panicked)
			if method == "tools/call" {
				result, err = &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: failure.Error()}},
				}, nil
				return
			}
			result, err = nil, failure
		}()
		return next(ctx, method, req)
	}
}

// limited makes a tool handler wait for a place among the calls the server
// works on at once, and hold it while it runs. A call the client gives up on
// while it waits is not run.
//
// The place is taken for each run of the handler rather than for the whole
// call. The question apply_plan puts to the administrator ends one run, and
// the answer starts another: the client sends it back on a retry of the
// call, or, for a client on an older protocol, the SDK asks and runs the
// handler again. So a call waiting for a person holds no place, and two
// questions left unanswered do not stop every other call.
//
// Each run reports its progress on a Bus of its own, made before the place
// is taken, so that the wait for one is a wait of the run's; the progress
// is sent to the client when it asked for it, and all of it before the
// handler returns. A tool is reported as a command named after it, but for
// read_command, whose command reports itself.
func limited[In, Out any](s *Server, handler mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (result *mcp.CallToolResult, out Out, err error) {
		ctx, done := s.watch(ctx, req)
		defer done()
		release, err := s.place(ctx, req.Params.Name)
		if err != nil {
			return nil, out, err
		}
		defer release()
		if req.Params.Name != readCommandTool {
			var span *progress.Span
			ctx, span = progress.Start(ctx, progress.KindCommand, req.Params.Name)
			returned := false
			defer func() {
				ended := err
				if !returned {
					ended = errPanicked
				}
				span.End(ended)
			}()
			result, out, err = handler(ctx, req, in)
			returned = true
			return result, out, err
		}
		return handler(ctx, req, in)
	}
}

// errPanicked ends the span of a tool whose handler panicked, which the
// recoverPanics middleware turns into a failed call.
var errPanicked = errors.New("the tool panicked")

// place waits for a place among the calls the server works on at once, as
// a wait of the call's when none is free, and returns what gives it back.
func (s *Server) place(ctx context.Context, tool string) (release func(), err error) {
	release = func() { <-s.calls }
	if ctx.Err() == nil {
		select {
		case s.calls <- struct{}{}:
			return release, nil
		default:
		}
	}
	_, wait := progress.Start(ctx, progress.KindWait, "waiting for another tool call",
		progress.Message(fmt.Sprintf("at most %d calls are worked on at once", maxCalls)))
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case s.calls <- struct{}{}:
		// When a place and the cancellation are both ready, select
		// picks either, so the context is asked again, once: the place
		// is given back on the answer the call is turned away with.
		if err = ctx.Err(); err != nil {
			<-s.calls
		}
	}
	if err != nil {
		wait.End(err)
		s.logf("%s was given up on while it waited for one of the %d calls worked on at once to end",
			tool, maxCalls)
		return nil, callError(fmt.Errorf(
			"cancelled while waiting for one of the %d calls the server works on at once to end: %w", maxCalls, err))
	}
	wait.End(nil)
	return release, nil
}

// callError renders an error for the agent, led by the kind of failure so
// that it can tell a mistake of its own from a host that did not answer.
func callError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", failureKind(err), err)
}

// failureKind names the exit code an error carries in words.
func failureKind(err error) string {
	switch exitcode.From(err) {
	case exitcode.Usage:
		return "rejected"
	case exitcode.Transport:
		return "unreachable"
	case exitcode.Interrupted:
		return "not confirmed"
	default:
		return "failed"
	}
}

// auditLog records every plan and every attempt to apply one, as one JSON
// object per line under the state directory.
type auditLog struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
}

// auditEntry is one line of the audit log. Its trace is that of the call
// it was written in, which the call's progress events belong to.
type auditEntry struct {
	Time    time.Time `json:"time"`
	Event   string    `json:"event"`
	Context string    `json:"context"`
	Trace   string    `json:"trace,omitempty"`
	Plan    string    `json:"plan,omitempty"`
	Action  string    `json:"action,omitempty"`
	Nodes   string    `json:"nodes,omitempty"`
	Count   int       `json:"count,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
}

func (l *auditLog) record(e auditEntry) error {
	e.Time = l.now().UTC()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := fileutil.EnsureDir(filepath.Dir(l.path)); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// record appends an entry to the audit log. A failure is logged and
// returned, so that the caller can refuse what it cannot record.
func (s *Server) record(e auditEntry) error {
	if err := s.audit.record(e); err != nil {
		s.logf("the audit log cannot be written: %v", err)
		return fmt.Errorf("the audit log %s cannot be written: %w", s.audit.path, err)
	}
	return nil
}

// refusal records a refused call and returns its error, noting when the
// refusal could not be recorded.
func (s *Server) refusal(e auditEntry, err error) error {
	if auditErr := s.record(e); auditErr != nil {
		return fmt.Errorf("%w (%w)", err, auditErr)
	}
	return err
}

// instructions tell the agent how the tools fit together.
func (s *Server) instructions() string {
	return strings.TrimSpace(fmt.Sprintf(`
clusterctl administers the HPC cluster %[2]s (context %[1]s). The server is
pinned to that context.

Nodes are named with ClusterShell node set expressions: exe[0001-0010],
ranges and lists like exe[1-4,7], groups like @rack:R02 or @slurm:main (a bare
@name uses the default group source), and the operators , (union),
! (difference), & (intersection) and ^ (symmetric difference), evaluated
strictly left to right. Resolve an expression with select_nodes before
acting on it.

Reading: select_nodes, describe_nodes and query_slurm answer the common
questions; read_command runs any other read-only clusterctl command and
lists them.

Changing: nothing is changed directly. Call plan_change to get a plan that
shows exactly which hosts are touched and what is sent, show it to the
user, then call apply_plan with the plan's id, nodes and count. The user is
asked to confirm; you cannot confirm for them. Protected hosts are refused
and cannot be forced from here. Running arbitrary commands on the nodes is
not offered.`, s.context, s.cluster))
}
