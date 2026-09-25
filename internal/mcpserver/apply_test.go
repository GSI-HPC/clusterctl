// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
	"github.com/GSI-HPC/clusterctl/internal/mcpserver"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// edit replaces the first occurrence of old in a file of the configuration.
func edit(t *testing.T, dir, file, old, replacement string) {
	t.Helper()
	path := filepath.Join(dir, file)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s does not contain %q", file, old)
	}
	updated := strings.Replace(string(data), old, replacement, 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

// auditLines returns the audit log, one decoded entry per line.
func auditLines(t *testing.T, stateDir string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "mcp", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// 10.1: the context the server is pinned to was pointed at another cluster
// while the plan waited. The user would have confirmed one cluster and the
// change would have run on the other.
func TestApplyRefusesAPlanWhoseClusterChanged(t *testing.T) {
	dir := configtest.CopyDir(t, exampleDir)
	f := start(t, setup{config: dir, answer: accept(map[string]any{"confirm": true})})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})

	edit(t, dir, "config.yaml", "cluster: cluster1", "cluster: cluster2")
	msg := f.refused(t, "apply_plan", applyArgs(p))
	if !strings.HasPrefix(msg, "rejected:") || !strings.Contains(msg, "cluster1") || !strings.Contains(msg, "cluster2") {
		t.Errorf("message = %q, want a rejection naming both clusters", msg)
	}
	if len(f.asked) != 0 {
		t.Errorf("the user was asked %q about a plan for another cluster", f.asked)
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v to the cluster the plan was not made for", sent)
	}
}

// 10.1: the host the Slurm clients run on changed while the plan waited, so
// what would be sent is no longer what the plan showed.
func TestApplyRefusesAPlanWhoseCommandsChanged(t *testing.T) {
	dir := configtest.CopyDir(t, exampleDir)
	f := start(t, setup{config: dir, answer: accept(map[string]any{"confirm": true})})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
	if len(p.Commands) != 1 || !strings.HasPrefix(p.Commands[0], "login (login.hpc.example.org): ") {
		t.Fatalf("commands = %q", p.Commands)
	}

	edit(t, dir, "site.yaml", "host: login.hpc.example.org", "host: wlm01.hpc.example.org")
	msg := f.refused(t, "apply_plan", applyArgs(p))
	if !strings.HasPrefix(msg, "rejected:") || !strings.Contains(msg, "wlm01.hpc.example.org") {
		t.Errorf("message = %q, want a rejection naming what would be sent now", msg)
	}
	if len(f.asked) != 0 {
		t.Errorf("the user was asked %q", f.asked)
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v", sent)
	}
}

// 10.10: safety.confirmAbove was lowered while the plan waited. The question
// is the one the gate asks now, so a plain yes no longer does.
func TestApplyAsksTheQuestionOfTheCurrentGate(t *testing.T) {
	dir := configtest.CopyDir(t, exampleDir)
	count := 4
	f := start(t, setup{config: dir, answer: func(req *mcp.ElicitRequest) *mcp.ElicitResult {
		if schema, _ := json.Marshal(req.Params.RequestedSchema); strings.Contains(string(schema), `"count"`) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"count": count}}
		}
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}
	}})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe[1-5]"})
	if p.CountRequired {
		t.Fatalf("plan = %+v, want no count required below 8 hosts", p)
	}

	edit(t, dir, "site.yaml", "confirmAbove: 8", "confirmAbove: 2")
	if msg := f.refused(t, "apply_plan", applyArgs(p)); !strings.HasPrefix(msg, "not confirmed:") {
		t.Errorf("5 hosts above a threshold of 2 = %q, want the count asked for and the wrong one refused", msg)
	}
	if len(f.asked) != 1 || !strings.Contains(f.asked[0], "Type the number of hosts") {
		t.Errorf("the user was asked %q, want the count", f.asked)
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v on the wrong count", sent)
	}

	count = 5
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	if !out.Applied {
		t.Errorf("result = %+v, want applied on the count", out)
	}
}

// 10.9: every refusal is recorded, not only the gate's.
func TestEveryRefusalIsAudited(t *testing.T) {
	f := start(t, setup{answer: accept(map[string]any{"confirm": true})})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe[1-2]"})
	refusals := []struct {
		tool string
		args map[string]any
	}{
		{"plan_change", map[string]any{"action": "reboot", "nodes": "exe1"}},
		{"plan_change", map[string]any{"action": "drain", "nodes": "exe1"}},
		{"plan_change", map[string]any{"action": "resume", "nodes": "exe["}},
		{"plan_change", map[string]any{"action": "resume", "nodes": "wlm01"}},
		{"apply_plan", map[string]any{"planId": "nope", "nodes": p.Nodes, "count": p.Count}},
		{"apply_plan", map[string]any{"planId": p.PlanID, "nodes": p.Nodes, "count": 3}},
		{"apply_plan", map[string]any{"planId": p.PlanID, "nodes": "exe[0001-0003]", "count": 2}},
	}
	for _, r := range refusals {
		f.refused(t, r.tool, r.args)
	}
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	f.refused(t, "apply_plan", applyArgs(p))

	var got []string
	for _, e := range auditLines(t, f.stateDir) {
		outcome, _ := e["outcome"].(string)
		got = append(got, e["event"].(string)+" "+strings.SplitN(outcome, ":", 2)[0])
	}
	want := []string{"plan planned",
		"plan refused", "plan refused", "plan refused", "plan refused",
		"apply refused", "apply refused", "apply refused",
		"apply applying", "apply applied",
		"apply refused"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audit =\n%v\nwant\n%v", got, want)
	}
}

// 10.9: a change that cannot be recorded is not made.
func TestApplyIsRefusedWhenTheAuditCannotBeWritten(t *testing.T) {
	f := start(t, setup{answer: accept(map[string]any{"confirm": true})})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})

	path := filepath.Join(f.stateDir, "mcp", "audit.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	msg := f.refused(t, "apply_plan", applyArgs(p))
	if !strings.Contains(msg, "audit") {
		t.Errorf("message = %q, want it to name the audit log", msg)
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v without a record", sent)
	}
	if msg := f.refused(t, "plan_change", map[string]any{"action": "resume", "nodes": "exe2"}); !strings.Contains(msg, "audit") {
		t.Errorf("a plan that cannot be recorded = %q, want it refused", msg)
	}
}

// runnerFunc adapts a function to transport.Runner.
type runnerFunc func(context.Context, transport.Target, transport.Request) (*transport.Result, error)

func (f runnerFunc) Run(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
	return f(ctx, target, req)
}

// 10.9: the client gave up on apply_plan after scontrol was sent. The change
// is carried through and recorded as it ended, rather than cut off and
// recorded as failed although Slurm took it.
func TestACancelledApplyStillCompletesAndIsRecorded(t *testing.T) {
	sent := make(chan struct{})
	f := start(t, setup{confirm: mcpserver.ConfirmApproval, runner: func(next transport.Runner) transport.Runner {
		return runnerFunc(func(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
			if len(req.Argv) == 0 || req.Argv[0] != "scontrol" {
				return next.Run(ctx, target, req)
			}
			close(sent)
			// ssh would be killed with the context; the change has
			// reached Slurm by then either way.
			select {
			case <-ctx.Done():
				return &transport.Result{Target: target, ExitCode: -1, Err: ctx.Err()}, nil
			case <-time.After(500 * time.Millisecond):
			}
			return next.Run(ctx, target, req)
		})
	}})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.session.CallTool(ctx, &mcp.CallToolParams{Name: "apply_plan", Arguments: applyArgs(p)})
		done <- err
	}()
	<-sent
	cancel()
	<-done

	deadline := time.Now().Add(5 * time.Second)
	for {
		lines := auditLines(t, f.stateDir)
		last := lines[len(lines)-1]
		if outcome, _ := last["outcome"].(string); last["event"] == "apply" && outcome != "applying" {
			if outcome != "applied" {
				t.Errorf("the audit records %q, want applied", outcome)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the apply never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.cluster.sent(); len(got) != 1 {
		t.Errorf("sent %v, want the resume", got)
	}
}
