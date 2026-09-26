// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/cli"
	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
	"github.com/GSI-HPC/clusterctl/internal/mcpserver"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// exampleDir is the configuration shipped with the documentation. Its
// protected hosts are wlm01 and dbm01, and above 8 hosts the count has to be
// typed.
const exampleDir = "../../examples/site"

// cluster fakes the Slurm clients on the login node and remembers every
// change sent to it.
type cluster struct {
	mu      sync.Mutex
	changes []string
}

func (c *cluster) reply(target transport.Target, req transport.Request) (*transport.Result, error) {
	result := &transport.Result{Target: target}
	if len(req.Argv) == 0 {
		return result, nil
	}
	switch req.Argv[0] {
	case "sinfo":
		result.Stdout = slurm.Render(req, slurmNodes(slurm.Arg(req, "--nodes"))...)
	case "squeue":
		result.Stdout = slurm.Render(req, slurm.Row{"i": "4711", "u": "alice", "a": "physics", "P": "main",
			"T": "RUNNING", "N": "exe0002", "D": "1", "C": "64", "l": "1-00:00:00", "M": "2:00:00", "Q": "100",
			"r": "None", "Z": "/home/alice", "o": "job.sh"})
	case "scontrol":
		c.mu.Lock()
		c.changes = append(c.changes, strings.Join(req.Argv, " "))
		c.mu.Unlock()
	}
	return result, nil
}

// slurmNodes returns the nodes sinfo lists for a --nodes argument: exe0001
// to exe0003 as they are, every other exe node idle, and nothing for a
// name Slurm does not know.
func slurmNodes(list string) []slurm.Row {
	known := []slurm.Row{
		{"N": "exe0001", "T": "drained", "R": "main", "c": "64", "E": "ticket 4711: DIMM", "u": "root", "H": "2026-09-01T10:00:00"},
		{"N": "exe0002", "T": "mixed", "R": "main", "c": "64", "E": "none", "u": "Unknown", "H": "Unknown"},
		{"N": "exe0003", "T": "idle", "R": "main", "c": "64", "E": "none", "u": "Unknown", "H": "Unknown"},
	}
	if list == "" {
		return known
	}
	var out []slurm.Row
	for _, name := range nodeset.MustParse(list).Expand() {
		switch {
		case name == "exe0001" || name == "exe0002" || name == "exe0003":
			out = append(out, known[name[len(name)-1]-'1'])
		case strings.HasPrefix(name, "exe"):
			out = append(out, slurm.Row{"N": name, "T": "idle", "R": "main", "c": "64", "E": "none"})
		}
	}
	return out
}

func (c *cluster) sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.changes...)
}

type setup struct {
	confirm  mcpserver.ConfirmMode
	answer   func(*mcp.ElicitRequest) *mcp.ElicitResult
	protocol string
	// config replaces the example configuration, for a test that edits it.
	config string
	// command replaces the command tree read_command runs.
	command func(context.Context, app.Streams) *cobra.Command
	// runner wraps the fake cluster, for a test that needs to see or delay
	// what is sent.
	runner func(transport.Runner) transport.Runner
	// force starts the server with --force, which it must not pass on.
	force bool
	// log receives the server's log; nil discards it.
	log io.Writer
	// ctx is what the server's side of the session is connected with, for
	// a test that gives the calls a progress Bus.
	ctx context.Context
	// wire records what goes between the client and the server.
	wire *wire
	// progress receives the progress notifications the client is sent.
	progress func(*mcp.ProgressNotificationParams)
}

type fixture struct {
	session  *mcp.ClientSession
	cluster  *cluster
	stateDir string
	asked    []string
}

func start(t *testing.T, s setup) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{cluster: &cluster{}, stateDir: filepath.Join(t.TempDir(), "state")}
	recorder := &transport.Recorder{Reply: f.cluster.reply}
	var runner transport.Runner = recorder
	if s.runner != nil {
		runner = s.runner(recorder)
	}
	config := s.config
	if config == "" {
		config = exampleDir
	}
	command := s.command
	if command == nil {
		command = cli.CommandTree(runner)
	}

	server, err := mcpserver.New(ctx, mcpserver.Options{
		App: app.Options{
			ConfigFiles: []string{config},
			Runner:      runner,
			Force:       s.force,
		},
		StateDir: f.stateDir,
		CacheDir: filepath.Join(t.TempDir(), "cache"),
		Command:  command,
		Confirm:  s.confirm,
		Version:  "test",
		Log:      s.log,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	opts := &mcp.ClientOptions{}
	if s.progress != nil {
		opts.ProgressNotificationHandler = func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			s.progress(req.Params)
		}
	}
	if s.answer != nil {
		opts.ElicitationHandler = func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			f.asked = append(f.asked, req.Params.Message)
			return s.answer(req), nil
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, opts)
	serverSide, clientSide := mcp.NewInMemoryTransports()
	connect := ctx
	if s.ctx != nil {
		connect = s.ctx
	}
	var transport mcp.Transport = serverSide
	if s.wire != nil {
		transport = s.wire.watch(serverSide)
	}
	if _, err := server.SDK().Connect(connect, transport, nil); err != nil {
		t.Fatalf("server Connect failed: %v", err)
	}
	session, err := client.Connect(ctx, clientSide, &mcp.ClientSessionOptions{ProtocolVersion: s.protocol})
	if err != nil {
		t.Fatalf("client Connect failed: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	f.session = session
	return f
}

// call runs a tool and decodes its structured result, failing the test when
// the tool reports an error.
func (f *fixture) call(t *testing.T, tool string, args map[string]any, out any) {
	t.Helper()
	res := f.callRaw(t, tool, args)
	if res.IsError {
		t.Fatalf("%s failed: %s", tool, text(res))
	}
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decoding %s: %v\n%s", tool, err, data)
	}
}

// refused runs a tool that must fail and returns its message.
func (f *fixture) refused(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	res := f.callRaw(t, tool, args)
	if !res.IsError {
		t.Fatalf("%s should have failed, returned %v", tool, res.StructuredContent)
	}
	return text(res)
}

func (f *fixture) callRaw(t *testing.T, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := f.session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func accept(content map[string]any) func(*mcp.ElicitRequest) *mcp.ElicitResult {
	return func(*mcp.ElicitRequest) *mcp.ElicitResult {
		return &mcp.ElicitResult{Action: "accept", Content: content}
	}
}

func TestToolsAreAnnotated(t *testing.T) {
	f := start(t, setup{})
	res, err := f.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := map[string]bool{}
	for _, tool := range res.Tools {
		readOnly[tool.Name] = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
	}
	want := map[string]bool{
		"select_nodes": true, "describe_nodes": true, "query_slurm": true,
		"read_command": true, "plan_change": true, "apply_plan": false,
	}
	for name, ro := range want {
		got, ok := readOnly[name]
		if !ok {
			t.Errorf("tool %s is missing", name)
			continue
		}
		if got != ro {
			t.Errorf("tool %s read-only = %v, want %v", name, got, ro)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("the server offers %d tools, want %d", len(res.Tools), len(want))
	}
}

func TestSelectNodesNamesProtectedAndUnknownHosts(t *testing.T) {
	f := start(t, setup{})
	var out struct {
		Context   string   `json:"context"`
		Nodes     string   `json:"nodes"`
		Count     int      `json:"count"`
		Expanded  []string `json:"expanded"`
		Unknown   string   `json:"unknown"`
		Protected string   `json:"protected"`
	}
	f.call(t, "select_nodes", map[string]any{"expression": "exe[1-3],wlm01,ghost1"}, &out)
	if out.Context != "cluster1" {
		t.Errorf("context = %q, want cluster1", out.Context)
	}
	if out.Count != 5 || len(out.Expanded) != 5 {
		t.Errorf("count = %d, expanded = %v, want 5", out.Count, out.Expanded)
	}
	if !strings.Contains(out.Nodes, "exe[0001-0003]") {
		t.Errorf("nodes = %q, want the inventory's names", out.Nodes)
	}
	if out.Protected != "wlm01" || out.Unknown != "ghost1" {
		t.Errorf("protected = %q, unknown = %q; want wlm01 and ghost1", out.Protected, out.Unknown)
	}

	msg := f.refused(t, "select_nodes", map[string]any{"expression": ""})
	if !strings.HasPrefix(msg, "rejected:") {
		t.Errorf("an empty selection should be rejected, got %q", msg)
	}
}

func TestDescribeNodesJoinsInventoryAndSlurm(t *testing.T) {
	f := start(t, setup{})
	var out struct {
		Items []struct {
			Name        string `json:"name"`
			Host        string `json:"host"`
			InInventory bool   `json:"inInventory"`
			Slurm       *struct {
				State  string `json:"state"`
				Reason string `json:"reason"`
			} `json:"slurm"`
			Jobs []struct {
				ID string `json:"id"`
			} `json:"jobs"`
		} `json:"items"`
		Errors map[string]string `json:"errors"`
	}
	f.call(t, "describe_nodes", map[string]any{"nodes": "exe[1-3]"}, &out)
	if len(out.Items) != 3 || len(out.Errors) != 0 {
		t.Fatalf("items = %+v, errors = %v", out.Items, out.Errors)
	}
	first, second := out.Items[0], out.Items[1]
	if !first.InInventory || first.Host == "" {
		t.Errorf("exe0001 = %+v, want it known with a host name", first)
	}
	if first.Slurm == nil || first.Slurm.Reason != "ticket 4711: DIMM" {
		t.Errorf("exe0001 slurm = %+v, want the drain reason", first.Slurm)
	}
	if len(second.Jobs) != 1 || second.Jobs[0].ID != "4711" {
		t.Errorf("exe0002 jobs = %+v, want job 4711", second.Jobs)
	}
}

func TestQuerySlurmFiltersAndLimits(t *testing.T) {
	f := start(t, setup{})
	var out struct {
		Count     int              `json:"count"`
		Truncated bool             `json:"truncated"`
		Items     []map[string]any `json:"items"`
	}
	f.call(t, "query_slurm", map[string]any{"kind": "nodes", "limit": 2}, &out)
	if out.Count != 3 || len(out.Items) != 2 || !out.Truncated {
		t.Errorf("count = %d, items = %d, truncated = %v; want 3, 2, true", out.Count, len(out.Items), out.Truncated)
	}
	f.call(t, "query_slurm", map[string]any{"kind": "summary"}, &out)
	if len(out.Items) != 1 || out.Items[0]["user"] != "alice" {
		t.Errorf("summary = %+v, want one row for alice", out.Items)
	}
	if msg := f.refused(t, "query_slurm", map[string]any{"kind": "everything"}); !strings.Contains(msg, "unknown kind") {
		t.Errorf("an unknown kind should be rejected, got %q", msg)
	}
}

func TestReadCommandRunsOnlyReadOnlyCommands(t *testing.T) {
	f := start(t, setup{})

	var out struct {
		Command  string `json:"command"`
		ExitCode int    `json:"exitCode"`
		Output   any    `json:"output"`
	}
	f.call(t, "read_command", map[string]any{"args": []string{"node", "select", "exe[1-3]"}}, &out)
	if out.Command != "node select" || out.ExitCode != 0 {
		t.Errorf("result = %+v", out)
	}
	names, _ := out.Output.([]any)
	if len(names) != 3 {
		t.Errorf("output = %v, want the three names as JSON", out.Output)
	}

	f.call(t, "read_command", map[string]any{"args": []string{"slurm", "node", "list", "--help"}}, &out)
	if s, _ := out.Output.(string); !strings.Contains(s, "--state") {
		t.Errorf("help output = %v, want the options", out.Output)
	}

	for _, args := range [][]string{
		{"slurm", "node", "drain", "why", "-n", "exe1"},
		{"exec", "-n", "exe1", "--", "reboot"},
		{"bmc", "power", "status", "-n", "exe1"},
		{"login"},
		{"mcp", "serve"},
		{"node", "list", "--context", "cluster2"},
		{"--context", "cluster2", "node", "list"},
		{"--set=fanout.max=1000", "slurm", "node", "list"},
		{"node", "list", "--config", "/tmp"},
		{"node", "list", "-y"},
		{"node", "list", "--force"},
		{"node", "list", "--progress", "counter"},
		{"slurm", "node"},
		{"nonsense"},
		{},
	} {
		msg := f.refused(t, "read_command", map[string]any{"args": args})
		if !strings.HasPrefix(msg, "rejected:") {
			t.Errorf("%v: message = %q, want a rejection", args, msg)
		}
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("a refused command sent %v", sent)
	}
}

type planResult struct {
	PlanID        string            `json:"planId"`
	Nodes         string            `json:"nodes"`
	Count         int               `json:"count"`
	Commands      []string          `json:"commands"`
	CommandLine   string            `json:"commandLine"`
	CurrentState  map[string]string `json:"currentState"`
	Warnings      []string          `json:"warnings"`
	CountRequired bool              `json:"countRequired"`
}

type applyResult struct {
	Applied bool   `json:"applied"`
	Message string `json:"message"`
}

func (f *fixture) plan(t *testing.T, args map[string]any) planResult {
	t.Helper()
	var p planResult
	f.call(t, "plan_change", args, &p)
	return p
}

func applyArgs(p planResult) map[string]any {
	return map[string]any{"planId": p.PlanID, "nodes": p.Nodes, "count": p.Count}
}

func TestPlanShowsWhatWouldBeSentAndSendsNothing(t *testing.T) {
	f := start(t, setup{})
	p := f.plan(t, map[string]any{"action": "drain", "nodes": "exe[1-3]", "reason": "ticket 4712: fans"})

	if p.Nodes != "exe[0001-0003]" || p.Count != 3 || p.CountRequired {
		t.Errorf("plan = %+v", p)
	}
	if len(p.Commands) != 1 || !strings.Contains(p.Commands[0], "scontrol update 'nodename=exe[0001-0003]' state=drain") {
		t.Errorf("commands = %v, want the scontrol update", p.Commands)
	}
	if !strings.Contains(p.CommandLine, "clusterctl slurm node drain 'ticket 4712: fans' -n 'exe[0001-0003]'") {
		t.Errorf("command line = %q", p.CommandLine)
	}
	if p.CurrentState["drained"] != "exe0001" || p.CurrentState["idle"] != "exe0003" {
		t.Errorf("current state = %v", p.CurrentState)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "exe0002 run jobs") {
		t.Errorf("warnings = %v, want the running job named", p.Warnings)
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("planning sent %v", sent)
	}
}

func TestPlanRefusesWhatTheGateRefuses(t *testing.T) {
	f := start(t, setup{})
	tests := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "drain", "nodes": "exe1"}, "needs a reason"},
		{map[string]any{"action": "drain", "nodes": "exe1,wlm01", "reason": "x"}, "protected host wlm01"},
		{map[string]any{"action": "reboot", "nodes": "exe1"}, "unknown action"},
		{map[string]any{"action": "resume", "nodes": ""}, "no nodes were selected"},
	}
	for _, tc := range tests {
		if msg := f.refused(t, "plan_change", tc.args); !strings.Contains(msg, tc.want) {
			t.Errorf("%v: message = %q, want %q", tc.args, msg, tc.want)
		}
	}
}

func TestApplyAsksTheUserAndSendsOnYes(t *testing.T) {
	for _, protocol := range []string{"", "2025-11-25"} {
		t.Run("protocol "+protocol, func(t *testing.T) {
			f := start(t, setup{answer: accept(map[string]any{"confirm": true}), protocol: protocol})
			p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})

			var out applyResult
			f.call(t, "apply_plan", applyArgs(p), &out)
			if !out.Applied {
				t.Errorf("result = %+v, want applied", out)
			}
			if len(f.asked) != 1 || !strings.Contains(f.asked[0], "About to resume 1 host: exe0001") {
				t.Errorf("the user was asked %q", f.asked)
			}
			sent := f.cluster.sent()
			if len(sent) != 1 || sent[0] != "scontrol update nodename=exe0001 state=resume" {
				t.Errorf("sent %v", sent)
			}

			// A plan is applied once.
			if msg := f.refused(t, "apply_plan", applyArgs(p)); !strings.Contains(msg, "there is no plan") {
				t.Errorf("a second apply = %q, want it refused", msg)
			}
		})
	}
}

func TestApplyChangesNothingWithoutTheUsersYes(t *testing.T) {
	tests := []struct {
		name   string
		answer *mcp.ElicitResult
	}{
		{"declined", &mcp.ElicitResult{Action: "decline"}},
		{"cancelled", &mcp.ElicitResult{Action: "cancel"}},
		{"unticked", &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": false}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := start(t, setup{answer: func(*mcp.ElicitRequest) *mcp.ElicitResult { return tc.answer }})
			p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
			if msg := f.refused(t, "apply_plan", applyArgs(p)); !strings.HasPrefix(msg, "not confirmed:") {
				t.Errorf("message = %q, want not confirmed", msg)
			}
			if sent := f.cluster.sent(); len(sent) != 0 {
				t.Errorf("sent %v without a yes", sent)
			}
		})
	}
}

func TestApplyAboveTheThresholdNeedsTheCount(t *testing.T) {
	var schema any
	f := start(t, setup{answer: func(req *mcp.ElicitRequest) *mcp.ElicitResult {
		schema = req.Params.RequestedSchema
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"count": 9}}
	}})
	p := f.plan(t, map[string]any{"action": "drain", "nodes": "exe[1-10]", "reason": "rack R02 maintenance"})
	if !p.CountRequired || p.Count != 10 {
		t.Fatalf("plan = %+v, want the count required for 10 hosts", p)
	}
	if msg := f.refused(t, "apply_plan", applyArgs(p)); !strings.HasPrefix(msg, "not confirmed:") {
		t.Errorf("the wrong count = %q, want not confirmed", msg)
	}
	if data, _ := json.Marshal(schema); !strings.Contains(string(data), `"count"`) {
		t.Errorf("the question asked for %s, want the count", data)
	}
	if !strings.Contains(strings.Join(f.asked, ""), "Type the number of hosts") {
		t.Errorf("the user was asked %q", f.asked)
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v on the wrong count", sent)
	}

	f = start(t, setup{answer: accept(map[string]any{"count": 10})})
	p = f.plan(t, map[string]any{"action": "drain", "nodes": "exe[1-10]", "reason": "rack R02 maintenance"})
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	if !out.Applied || len(f.cluster.sent()) != 1 {
		t.Errorf("result = %+v, sent %v; want it applied", out, f.cluster.sent())
	}
}

func TestApplyNeedsThePlanRepeated(t *testing.T) {
	f := start(t, setup{answer: accept(map[string]any{"confirm": true})})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe[1-2]"})
	for _, args := range []map[string]any{
		{"planId": p.PlanID, "nodes": p.Nodes, "count": 3},
		{"planId": p.PlanID, "nodes": "exe[0001-0003]", "count": 2},
		{"planId": "nope", "nodes": p.Nodes, "count": 2},
	} {
		if msg := f.refused(t, "apply_plan", args); !strings.HasPrefix(msg, "rejected:") {
			t.Errorf("%v: message = %q, want rejected", args, msg)
		}
	}
	if len(f.asked) != 0 || len(f.cluster.sent()) != 0 {
		t.Errorf("asked %q and sent %v for a plan that was not repeated", f.asked, f.cluster.sent())
	}
}

func TestApplyWithoutElicitationDependsOnTheMode(t *testing.T) {
	f := start(t, setup{})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
	msg := f.refused(t, "apply_plan", applyArgs(p))
	if !strings.Contains(msg, "cannot ask the user") || !strings.Contains(msg, "clusterctl slurm node resume") {
		t.Errorf("message = %q, want the refusal and the command to run instead", msg)
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v without a way to ask", sent)
	}

	f = start(t, setup{confirm: mcpserver.ConfirmApproval})
	p = f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	if !out.Applied || len(f.cluster.sent()) != 1 {
		t.Errorf("result = %+v, sent %v; want it applied on the client's approval", out, f.cluster.sent())
	}
}

func TestPlansAndAppliesAreAudited(t *testing.T) {
	f := start(t, setup{answer: accept(map[string]any{"confirm": true})})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	f.refused(t, "plan_change", map[string]any{"action": "resume", "nodes": "wlm01"})

	data, err := os.ReadFile(filepath.Join(f.stateDir, "mcp", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var e struct {
			Event   string    `json:"event"`
			Plan    string    `json:"plan"`
			Outcome string    `json:"outcome"`
			Time    time.Time `json:"time"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		if e.Time.IsZero() {
			t.Errorf("audit line %q has no time", line)
		}
		outcomes = append(outcomes, e.Event+" "+strings.SplitN(e.Outcome, ":", 2)[0])
	}
	want := []string{"plan planned", "apply applying", "apply applied", "plan refused"}
	if strings.Join(outcomes, ",") != strings.Join(want, ",") {
		t.Errorf("audit = %v, want %v", outcomes, want)
	}
}

// describe_nodes showed the name the naming rules derive, not the bmcAddress
// the bmc commands reach, and left the field empty without a word when a
// node has no service processor name at all.
func TestDescribeNodesShowsTheBMCTheCommandsReach(t *testing.T) {
	type view struct {
		Items []struct {
			Name     string `json:"name"`
			BMC      string `json:"bmc"`
			BMCError string `json:"bmcError"`
		} `json:"items"`
	}

	dir := configtest.CopyDir(t, exampleDir)
	edit(t, dir, "inventory.yaml", "    - nodes: exe0001\n",
		"    - nodes: exe0003\n      bmcAddress: 10.9.0.77\n    - nodes: exe0001\n")
	var out view
	start(t, setup{config: dir}).call(t, "describe_nodes",
		map[string]any{"nodes": "exe[3-4]", "facets": []string{"inventory"}}, &out)
	if len(out.Items) != 2 {
		t.Fatalf("items = %+v", out.Items)
	}
	if got := out.Items[0]; got.BMC != "10.9.0.77" || got.BMCError != "" {
		t.Errorf("exe0003 = %+v, want its BMC shown as 10.9.0.77", got)
	}
	if got := out.Items[1]; got.BMC != "exe0004.mgmt.hpc.example.org" || got.BMCError != "" {
		t.Errorf("exe0004 = %+v, want its BMC shown by its derived name", got)
	}

	// Without a bmc template, a node without a bmcAddress has no BMC, and
	// the view says why.
	edit(t, dir, "site.yaml", "        bmc: \"{name}.{domains.mgmtHpc}\"\n", "")
	out = view{}
	start(t, setup{config: dir}).call(t, "describe_nodes",
		map[string]any{"nodes": "exe[3-4]", "facets": []string{"inventory"}}, &out)
	if len(out.Items) != 2 {
		t.Fatalf("items = %+v", out.Items)
	}
	if got := out.Items[0]; got.BMC != "10.9.0.77" || got.BMCError != "" {
		t.Errorf("exe0003 = %+v, want its BMC shown as 10.9.0.77", got)
	}
	if got := out.Items[1]; got.BMC != "" || !strings.Contains(got.BMCError, "bmcAddress") {
		t.Errorf("exe0004 = %+v, want no BMC and the reason", got)
	}
}
