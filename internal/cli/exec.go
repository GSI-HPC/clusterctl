// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func newExecCommand(r *root) *cobra.Command {
	var (
		user    string
		root    bool
		dedup   bool
		script  string
		stdin   bool
		timeout time.Duration
		confirm bool
	)

	cmd := leaf("exec [-n NODESET] -- COMMAND...", "Run one command on many nodes at once", `
Run a command on every node of a set, in parallel.

The argument vector is quoted once and reassembled by the remote shell, so it
arrives exactly as it was typed. A timeout is enforced on the node with
timeout(1), because killing the local ssh would leave the remote process
running.

  clusterctl exec -n @idle -- uptime
  clusterctl exec -n exe[1-10] --dedup -- uname -r
  clusterctl exec -n exe[1-4] --script 'systemctl is-active slurmd || journalctl -u slurmd -n 5'

With --dedup the nodes that answered the same thing are collapsed into one
block, which turns a thousand replies into the few worth reading.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}

			argv := args
			if at := cmd.ArgsLenAtDash(); at >= 0 {
				argv = args[at:]
			}
			if len(argv) == 0 && script == "" {
				return exitcode.Errorf(exitcode.Usage,
					"nothing to run; give a command after -- or use --script")
			}
			if len(argv) > 0 && script != "" {
				return exitcode.Errorf(exitcode.Usage, "--script and a command after -- contradict each other")
			}

			ns, err := a.Select("")
			if err != nil {
				return err
			}
			targets, err := a.NodeTargets(ns)
			if err != nil {
				return err
			}
			for i := range targets {
				if user != "" {
					targets[i].User = user
				}
				if root {
					targets[i].User = "root"
				}
			}

			limit := timeout
			if limit == 0 {
				limit = a.Timeout().Get()
			}
			req := transport.Request{Argv: argv, Script: script, Timeout: limit, TTY: transport.TTYNone}
			if stdin {
				// One stdin cannot be shared by many nodes, so it is read
				// once and replayed to each of them.
				payload, err := readAll(a)
				if err != nil {
					return err
				}
				req.Stdin = nil
				return runWithPayload(a, cmd, targets, req, payload, dedup)
			}

			if confirm || a.DryRun() {
				detail := req.Script
				if detail == "" {
					detail = strings.Join(argv, " ")
				}
				err := a.Gate.Confirm(safety.Action{Verb: "run a command on", Targets: ns, Detail: detail})
				if err != nil {
					return err
				}
			}

			results := a.Executor().Run(a.Context(), targets, req)
			return printExec(a, cmd, results, dedup)
		})

	flags := cmd.Flags()
	flags.StringVarP(&user, "user", "u", "", "remote account to run as")
	flags.BoolVarP(&root, "root", "r", false, "run as root")
	flags.BoolVarP(&dedup, "dedup", "b", false, "collapse nodes that answered the same thing")
	flags.StringVar(&script, "script", "", "shell program to run instead of a command")
	flags.BoolVar(&stdin, "stdin", false, "read standard input once and send it to every node")
	flags.DurationVar(&timeout, "timeout", 0, "how long the command may run on a node (default: from the configuration)")
	flags.BoolVar(&confirm, "confirm", false, "ask before running, as the destructive commands do")
	return cmd
}

// runWithPayload sends the same standard input to every node.
func runWithPayload(a *app.App, cmd *cobra.Command, targets []transport.Target, req transport.Request, payload []byte, dedup bool) error {
	results := a.Executor().RunEach(a.Context(), targets, func(transport.Target) transport.Request {
		out := req
		out.Stdin = strings.NewReader(string(payload))
		return out
	})
	return printExec(a, cmd, results, dedup)
}

func readAll(a *app.App) ([]byte, error) {
	if a.In == nil {
		return nil, exitcode.Errorf(exitcode.Usage, "--stdin was given but there is nothing to read")
	}
	var b strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := a.In.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			if n == 0 {
				break
			}
		}
		if n == 0 {
			break
		}
	}
	return []byte(b.String()), nil
}

// printExec renders what the nodes answered.
//
// For a terminal the raw output is streamed with the node in front of each
// line, which is what an administrator reads; the machine formats get the
// structured result instead.
func printExec(a *app.App, cmd *cobra.Command, results []*transport.Result, dedup bool) error {
	if a.Format.IsMachine() {
		if err := a.Print(output.Result{Object: results, Table: resultsTable(results)}); err != nil {
			return err
		}
		return failureError(results)
	}

	out := cmd.OutOrStdout()
	if dedup {
		for _, g := range fanout.GroupByOutput(results) {
			if _, err := fmt.Fprintf(out, "%s (%d)\n", g.Nodes, g.Nodes.Len()); err != nil {
				return err
			}
			for _, line := range strings.Split(g.Output, "\n") {
				if _, err := fmt.Fprintf(out, "  %s\n", line); err != nil {
					return err
				}
			}
		}
	} else {
		for _, res := range results {
			for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
				if line == "" && res.Stdout == "" {
					continue
				}
				if _, err := fmt.Fprintf(out, "%s: %s\n", res.Target.Name, line); err != nil {
					return err
				}
			}
		}
	}
	for _, res := range results {
		if res.Failed() {
			detail := strings.TrimSpace(lastNonEmpty(res.Stderr))
			if res.Err != nil {
				detail = res.Err.Error()
			}
			fmt.Fprintf(os.Stderr, "%s: %s\n", res.Target.Name, detail)
		}
	}
	return failureError(results)
}
