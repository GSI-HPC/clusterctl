// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// wire records what the server reads from the client and writes to it, in
// the order it writes.
type wire struct {
	mu sync.Mutex
	// tokens are the progress tokens of the calls, by request id.
	tokens map[string]string
	// written are what the server wrote: a notification's token, or the
	// token of the call a response answers, with "result" as its kind.
	written []written
}

type written struct {
	kind, token string
	progress    mcp.ProgressNotificationParams
}

func (w *wire) watch(t mcp.Transport) mcp.Transport { return wireTransport{t, w} }

type wireTransport struct {
	mcp.Transport
	w *wire
}

func (t wireTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return wireConnection{c, t.w}, nil
}

type wireConnection struct {
	mcp.Connection
	w *wire
}

func (c wireConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "tools/call" {
		var params struct {
			Meta struct {
				ProgressToken any `json:"progressToken"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(req.Params, &params)
		c.w.mu.Lock()
		if c.w.tokens == nil {
			c.w.tokens = map[string]string{}
		}
		c.w.tokens[fmt.Sprint(req.ID.Raw())] = fmt.Sprint(params.Meta.ProgressToken)
		c.w.mu.Unlock()
	}
	return msg, err
}

// Write keeps what is written, in the order it goes out: the lock is held
// across the write.
func (c wireConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	switch m := msg.(type) {
	case *jsonrpc.Request:
		if m.Method == "notifications/progress" {
			var p mcp.ProgressNotificationParams
			if err := json.Unmarshal(m.Params, &p); err != nil {
				return err
			}
			c.w.written = append(c.w.written, written{kind: "progress", token: fmt.Sprint(p.ProgressToken), progress: p})
		}
	case *jsonrpc.Response:
		if token, ok := c.w.tokens[fmt.Sprint(m.ID.Raw())]; ok {
			c.w.written = append(c.w.written, written{kind: "result", token: token})
		}
	}
	return c.Connection.Write(ctx, msg)
}

// sent returns the notifications sent for token and how many of them went
// out after the call's result.
func (w *wire) sent(token string) (notes []mcp.ProgressNotificationParams, late int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	answered := false
	for _, e := range w.written {
		if e.token != token {
			continue
		}
		switch {
		case e.kind == "result":
			answered = true
		case answered:
			late++
		default:
			notes = append(notes, e.progress)
		}
	}
	return notes, late
}

// withToken runs a tool with a progress token.
func (f *fixture) withToken(t *testing.T, token, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	params.SetProgressToken(token)
	res, err := f.session.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s failed: %s", tool, text(res))
	}
	return res
}

// probing is a command tree whose probe works on as many nodes as its
// argument says, one at a time, each for pause, as the step "probe the
// nodes".
func probing(pause time.Duration) func(context.Context, app.Streams) *cobra.Command {
	return tree(func(cmd *cobra.Command) error {
		n, err := strconv.Atoi(cmd.Flags().Arg(0))
		if err != nil {
			return err
		}
		nodes := make([]string, n)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("exe%d", i+1)
		}
		fanout.Map(cmd.Context(), nodes, fanout.Options[string]{Step: "probe the nodes", Limit: 1},
			func(context.Context, string) (struct{}, error) {
				time.Sleep(pause)
				return struct{}{}, nil
			})
		return nil
	})
}

// checkProgress fails the test unless the notifications count up to total:
// the targets ended grow with every notification, as MCP asks, and never
// pass what is expected, which never falls, and the last says all of it
// ended.
func checkProgress(t *testing.T, token string, notes []mcp.ProgressNotificationParams, total float64) {
	t.Helper()
	if len(notes) == 0 {
		t.Fatalf("%s: no progress was sent", token)
	}
	progress, expected := -1.0, 0.0
	for i, n := range notes {
		if n.Progress <= progress || n.Total < expected || n.Progress > n.Total {
			t.Errorf("%s: notification %d says %v of %v after %v of %v", token, i, n.Progress, n.Total, progress, expected)
		}
		progress, expected = n.Progress, n.Total
	}
	if last := notes[len(notes)-1]; last.Progress != total || last.Total != total {
		t.Errorf("%s: the last notification says %v of %v, want %v of %v", token, last.Progress, last.Total, total, total)
	}
}

// A call's progress is sent as it goes, at most every half second and as a
// step ends, counted up to what the call expects and never back, and all
// of it before the call's result.
func TestProgressIsSentAsTheCallGoesAndBeforeItsResult(t *testing.T) {
	w := &wire{}
	var received []mcp.ProgressNotificationParams
	var mu sync.Mutex
	f := start(t, setup{wire: w, command: probing(150 * time.Millisecond), progress: func(p *mcp.ProgressNotificationParams) {
		mu.Lock()
		received = append(received, *p)
		mu.Unlock()
	}})
	f.withToken(t, "probe", "read_command", map[string]any{"args": []string{"probe", "6"}})
	notes, late := w.sent("probe")
	checkProgress(t, "probe", notes, 6)
	if late != 0 {
		t.Errorf("%d notifications went out after the result", late)
	}
	if len(notes) < 3 || len(notes) > 6 {
		t.Errorf("%d notifications for a call of about a second, want one at its start, one or two while it ran and one as its step ended: %+v", len(notes), notes)
	}
	for _, n := range notes {
		if !regexp.MustCompile(`^probe the nodes: \d/6 done$`).MatchString(n.Message) {
			t.Errorf("message %q, want the step and how far it has got", n.Message)
		}
	}
	// The client is handed them in the order they were sent.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got >= len(notes) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(received) != fmt.Sprint(notes) {
		t.Errorf("the client received\n%+v\nwant\n%+v", received, notes)
	}
}

// What a call leaves open is ended as its Bus is closed, as the handler
// returns, and counted in the last notification, which goes out before the
// call's result all the same.
func TestProgressCountsWhatTheCallLeftOpen(t *testing.T) {
	w := &wire{}
	f := start(t, setup{wire: w, command: tree(func(cmd *cobra.Command) error {
		// The step and three of its targets are left open.
		ctx, _ := progress.Start(cmd.Context(), progress.KindStep, "probe the nodes",
			progress.WithFlags(progress.Fold), progress.Total(4))
		targets := make([]*progress.Span, 4)
		for i := range targets {
			node := fmt.Sprintf("exe%d", i+1)
			_, targets[i] = progress.Start(ctx, progress.KindTarget, node, progress.Queued(), progress.Node(node))
		}
		targets[0].Run()
		targets[0].End(nil)
		return nil
	})})
	for i := range 5 {
		token := fmt.Sprintf("open %d", i)
		f.withToken(t, token, "read_command", probe)
		notes, late := w.sent(token)
		checkProgress(t, token, notes, 4)
		if late != 0 {
			t.Errorf("%s: %d notifications went out after the result", token, late)
		}
	}
}

// A step that starts once another has ended raises what is expected, but
// no notification says so until more targets have ended, since each must
// say more are done than the last; and a line about the step under way
// says what the numbers count in all once they count more than it.
func TestProgressOfStepsInTurnOnlyGrows(t *testing.T) {
	w := &wire{}
	f := start(t, setup{wire: w, command: tree(func(cmd *cobra.Command) error {
		for _, name := range []string{"read the power state", "read the uptime"} {
			fanout.Map(cmd.Context(), []string{"exe1", "exe2"}, fanout.Options[string]{Step: name, Limit: 1},
				func(context.Context, string) (struct{}, error) { return struct{}{}, nil })
		}
		return nil
	})})
	f.withToken(t, "in turn", "read_command", probe)
	notes, late := w.sent("in turn")
	checkProgress(t, "in turn", notes, 4)
	if late != 0 {
		t.Errorf("%d notifications went out after the result", late)
	}
	for _, n := range notes {
		if n.Total > 2 && !strings.HasSuffix(n.Message, fmt.Sprintf("; %v/%v in all", n.Progress, n.Total)) {
			t.Errorf("%v of %v: %q, want the line to end with what the numbers count in all", n.Progress, n.Total, n.Message)
		}
	}
	if last := notes[len(notes)-1].Message; last != "read the uptime: 2/2 done; 4/4 in all" {
		t.Errorf("the last line is %q", last)
	}
}

// Two calls side by side each have a Bus of their own: each call's
// notifications count its own work, never the other's.
func TestTheProgressOfCallsSideBySideDoesNotMix(t *testing.T) {
	w := &wire{}
	f := start(t, setup{wire: w, command: probing(20 * time.Millisecond)})
	var wg sync.WaitGroup
	for _, n := range []string{"3", "7"} {
		wg.Go(func() { f.withToken(t, "probe "+n, "read_command", map[string]any{"args": []string{"probe", n}}) })
	}
	wg.Wait()
	for token, total := range map[string]float64{"probe 3": 3, "probe 7": 7} {
		notes, late := w.sent(token)
		checkProgress(t, token, notes, total)
		if late != 0 {
			t.Errorf("%s: %d notifications went out after the result", token, late)
		}
	}
}

// A client that does not ask for a call's progress is sent none.
func TestNoProgressIsSentWithoutAToken(t *testing.T) {
	w := &wire{}
	f := start(t, setup{wire: w, command: probing(0)})
	var out commandResult
	f.call(t, "read_command", map[string]any{"args": []string{"probe", "3"}}, &out)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.written {
		if e.kind == "progress" {
			t.Errorf("a notification was sent for a call without a progress token: %+v", e.progress)
		}
	}
}

// What a command returns, its notes among them, is the same whether its
// progress is sent or not.
func TestProgressLeavesTheNotesAlone(t *testing.T) {
	w := &wire{}
	f := start(t, setup{wire: w, runner: func(next transport.Runner) transport.Runner {
		return runnerFunc(func(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
			if target.Name == "exe0002" {
				return transport.ExitResult(target, 1, "", "dmidecode: command not found\n"), nil
			}
			return next.Run(ctx, target, req)
		})
	}})
	args := map[string]any{"args": []string{"node", "hw", "-n", "exe[1-3]", "-o", "table"}}
	var quiet, watched commandResult
	f.call(t, "read_command", args, &quiet)
	res := f.withToken(t, "node hw", "read_command", args)
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &watched); err != nil {
		t.Fatal(err)
	}
	if quiet.Notes == "" || quiet.ExitCode == 0 {
		t.Fatalf("node hw = %+v; the test wants notes and a failure", quiet)
	}
	if fmt.Sprint(watched) != fmt.Sprint(quiet) {
		t.Errorf("with its progress sent, read_command returned\n%+v\nwant\n%+v", watched, quiet)
	}
	notes, _ := w.sent("node hw")
	checkProgress(t, "node hw", notes, 3)
}

// Each plan and apply is recorded with the trace of its call, which the
// call's progress events belong to, so that an audit line can be joined to
// them: a plan and its apply are two calls, and the two lines of one apply
// one.
func TestTheAuditLogNamesTheTraceOfEachCall(t *testing.T) {
	f := start(t, setup{answer: accept(map[string]any{"confirm": true})})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	lines := auditLines(t, f.stateDir)
	if len(lines) != 3 {
		t.Fatalf("audit log = %v, want the plan, applying and applied", lines)
	}
	hex := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, line := range lines {
		if trace, _ := line["trace"].(string); !hex.MatchString(trace) {
			t.Errorf("audit line %v has no trace", line)
		}
	}
	if lines[0]["trace"] == lines[1]["trace"] || lines[1]["trace"] != lines[2]["trace"] {
		t.Errorf("traces %v, %v, %v; want the plan's its own, and the apply's one for both its lines",
			lines[0]["trace"], lines[1]["trace"], lines[2]["trace"])
	}

	// A Bus the calls are given, as a test's, is the one whose trace they
	// record.
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	watched := start(t, setup{ctx: progress.WithBus(context.Background(), bus)})
	watched.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
	bus.Close()
	progresstest.Check(t, c.Events())
	if got := auditLines(t, watched.stateDir)[0]["trace"]; got != bus.Trace().String() {
		t.Errorf("the plan recorded the trace %v, want its Bus's %s", got, bus.Trace())
	}
	if tree := c.Tree(); !strings.HasPrefix(tree, "command plan_change: ok\n") {
		t.Errorf("progress:\n%s\nwant the call as a command of its own", tree)
	}
}

// A call that has to wait for a place reports the wait; a tool is a
// command of its own, named after it, but for read_command, whose command
// reports itself.
func TestACallWaitingForAPlaceReportsTheWait(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	f := start(t, setup{ctx: progress.WithBus(context.Background(), bus), command: tree(func(*cobra.Command) error {
		entered <- struct{}{}
		<-release
		return nil
	})})
	var wg sync.WaitGroup
	wg.Go(func() { f.callAtOnce(t, 2, probe) })
	<-entered
	<-entered
	wg.Go(func() {
		var out struct {
			Count int `json:"count"`
		}
		f.call(t, "select_nodes", map[string]any{"expression": "exe[1-3]"}, &out)
	})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.Tree(), "wait waiting for another tool call") {
		if time.Now().After(deadline) {
			t.Fatalf("no wait was reported:\n%s", c.Tree())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	bus.Close()
	progresstest.Check(t, c.Events())
	want := `command select_nodes: ok
wait waiting for another tool call message=at most 2 calls are worked on at once: ok
`
	if got := c.Tree(); got != want {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want)
	}
}
