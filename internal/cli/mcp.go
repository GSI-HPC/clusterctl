// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/mcpserver"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/internal/version"
)

func newMCPCommand(r *root) *cobra.Command {
	return group("mcp", "Offer clusterctl to an AI agent", `
Serve the Model Context Protocol, so that an agent in an MCP client such as
Claude Code can read the state of the cluster and propose changes.`,
		newMCPServeCommand(r),
	)
}

func newMCPServeCommand(r *root) *cobra.Command {
	var (
		confirm string
		planTTL time.Duration
	)
	cmd := leaf("serve", "Serve the Model Context Protocol over standard input and output", `
Run an MCP server on standard input and output for a client started on this
workstation. It acts as you: your configuration, your ssh agent, and the
context it was started in, which it keeps for its whole life.

The agent can resolve node sets, describe nodes, query Slurm and run any
command that only reads. It cannot change anything directly. A change is
planned first, which resolves the node set once, refuses protected hosts and
shows exactly what would be sent; applying the plan puts the same question
the confirmation prompt asks to you, through the client, and the agent
cannot answer it. Every plan and every attempt to apply one is recorded in
mcp/audit.jsonl under the state directory.

With --confirm=approval the question is left to the client's own approval
of the apply_plan call instead, for a client that cannot ask. Only use it
with a client that asks you before every call of that tool.

Commands on the nodes cannot be run, and --yes, --force, --dry-run and
--nodes are refused: the server never confirms or forces anything itself.

Register it with Claude Code:

  claude mcp add clusterctl -- clusterctl mcp serve --context prod`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			if r.assumeYes || r.force || r.dryRun || len(r.nodes) > 0 {
				return exitcode.Errorf(exitcode.Usage,
					"mcp serve does not take --yes, --force, --dry-run or --nodes; the server never confirms or forces anything itself")
			}
			set := map[string]string{}
			for _, assignment := range r.setValues {
				key, value, _ := strings.Cut(assignment, "=")
				set[strings.TrimSpace(key)] = value
			}
			server, err := mcpserver.New(r.context(), mcpserver.Options{
				App: app.Options{
					ConfigFiles: r.configFiles,
					Context:     r.contextName,
					Set:         set,
					Fanout:      r.fanout,
					Runner:      r.runner,
				},
				StateDir: r.streams.StateDir,
				CacheDir: r.streams.CacheDir,
				Command:  CommandTree(r.runner),
				Confirm:  mcpserver.ConfirmMode(confirm),
				PlanTTL:  planTTL,
				Version:  version.Get().Version,
				Log:      r.streams.Err,
			})
			if err != nil {
				return err
			}
			return server.Run(r.context(), &mcp.StdioTransport{})
		})
	cmd.Flags().StringVar(&confirm, "confirm", string(mcpserver.ConfirmElicit),
		"who confirms a change: "+strings.Join(mcpserver.ConfirmModes(), " or "))
	cmd.Flags().DurationVar(&planTTL, "plan-ttl", 10*time.Minute, "how long a plan may wait to be applied")
	_ = cmd.RegisterFlagCompletionFunc("confirm", fixed(mcpserver.ConfirmModes()...))
	return cmd
}

// CommandTree returns a function that builds a fresh command tree writing to
// the given streams, for the MCP server's read-only commands. Each call gets a
// tree of its own, so calls running at the same time share no flag state. A
// non-nil runner replaces the ssh transport, as it does in the tests.
func CommandTree(runner transport.Runner) func(context.Context, app.Streams) *cobra.Command {
	return func(ctx context.Context, streams app.Streams) *cobra.Command {
		cmd, r := newRoot(ctx, streams)
		r.runner = runner
		cmd.SetIn(streams.In)
		cmd.SetOut(streams.Out)
		cmd.SetErr(streams.Err)
		return cmd
	}
}
