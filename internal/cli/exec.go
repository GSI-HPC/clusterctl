// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

	cmd := leaf("exec [NODESET] -- COMMAND...", "Run one command on many nodes at once", `
Run a command on every node of a set, in parallel.

The command follows --, so that none of its options is read as one of
clusterctl's: without it, a -r or -n meant for the node would change what
clusterctl does. The node set goes before --, or in -n, but not in both.

The argument vector is quoted once and reassembled by the remote shell, so it
arrives exactly as it was typed. A timeout is enforced on the node with
timeout(1), because killing the local ssh would leave the remote process
running. A node that has not answered five seconds after the timeout, plus the
time ssh.connectTimeout and ssh.connectionAttempts let reaching it take, has
stopped answering: ssh is stopped here, and the node is reported as
unreachable.

  clusterctl exec -n @slurm:main -- uptime
  clusterctl exec exe[1-10] --dedup -- uname -r
  clusterctl exec -n exe[1-4] --script 'systemctl is-active slurmd || journalctl -u slurmd -n 5'

A protected host is refused unless --force is given, on every run. exec does
not ask before it runs, unless --confirm is given. With --stdin the payload
takes up standard input, so the confirmation cannot be read from it and
--confirm needs -y.

With --dedup the nodes that answered the same thing, and ended the same way,
are collapsed into one block, which turns a thousand replies into the few
worth reading. Control characters in what the nodes answered are shown as
escapes rather than passed to the terminal.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			// Only the words after -- are the command, so that none of its
			// options can be read as clusterctl's own. The words before it
			// are the node set, as for every other node command. The shape
			// of the command line is checked before anything else, so that
			// a -n meant for the node is reported as a missing --.
			nodes, argv := args, []string(nil)
			if at := cmd.ArgsLenAtDash(); at >= 0 {
				nodes, argv = args[:at], args[at:]
			} else if len(args) > 0 && script == "" {
				return exitcode.Errorf(exitcode.Usage,
					"the command has to follow --, so that its options are not read as clusterctl's: "+
						"clusterctl exec [-n NODESET] -- COMMAND...")
			}
			if len(argv) == 0 && script == "" {
				return exitcode.Errorf(exitcode.Usage,
					"nothing to run; give a command after -- or use --script")
			}
			if len(argv) > 0 && script != "" {
				return exitcode.Errorf(exitcode.Usage, "--script and a command after -- contradict each other")
			}
			if len(nodes) > 0 && cmd.Flags().Changed("nodes") {
				return exitcode.Errorf(exitcode.Usage,
					"%q is a node set, and -n already names one; give the nodes once, and the command after --",
					strings.Join(nodes, " "))
			}

			a, err := r.App()
			if err != nil {
				return err
			}
			if stdin && confirm && !a.Gate.AssumeYes && !a.DryRun() {
				return exitcode.Errorf(exitcode.Usage,
					"--stdin carries the payload, so the confirmation --confirm asks for cannot be read from it; "+
						"pass -y to confirm in advance")
			}

			ns, err := selection(a, nodes)
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

			req := a.Collect(transport.Request{Argv: argv, Script: script, Timeout: timeout})

			action := safety.Action{Verb: "run a command on", Targets: ns, Detail: req.Script}
			if action.Detail == "" {
				action.Detail = strings.Join(argv, " ")
			}
			// The protected hosts are refused on every run. Only the
			// question depends on --confirm, so that a dry run and a real
			// run make the same decision.
			if err := a.Gate.Check(action); err != nil {
				return err
			}

			var payload []byte
			if stdin {
				// One stdin cannot be shared by many nodes, so it is read
				// once and replayed to each of them.
				if payload, err = readAll(a); err != nil {
					return err
				}
				action.Detail += fmt.Sprintf("  (with %d bytes on standard input)", len(payload))
			}

			if confirm || a.DryRun() {
				if err := a.Gate.Confirm(action); err != nil {
					return err
				}
			} else if err := a.Gate.Announce(action); err != nil {
				// Nothing is asked, but what --force lets through is
				// still named.
				return err
			}

			var results []*transport.Result
			if stdin {
				results = runWithPayload(a.Context(), a, targets, req, payload)
			} else {
				results = a.Executor().Run(a.Context(), targets, req)
			}
			return printExec(a, cmd, results, dedup)
		})

	flags := cmd.Flags()
	flags.StringVarP(&user, "user", "u", "", "remote account to run as")
	flags.BoolVarP(&root, "root", "r", false, "run as root")
	flags.BoolVarP(&dedup, "dedup", "b", false, "collapse nodes that answered the same thing")
	flags.StringVar(&script, "script", "", "shell program to run instead of a command")
	flags.BoolVar(&stdin, "stdin", false, "read standard input once and send it to every node")
	flags.DurationVar(&timeout, "timeout", 0, "how long the command may run on a node (default: from the configuration)")
	flags.BoolVar(&confirm, "confirm", false, "ask before running, as the destructive commands do; with --stdin, give -y as well")
	return cmd
}

// runWithPayload sends the same standard input to every node.
func runWithPayload(ctx context.Context, a *app.App, targets []transport.Target, req transport.Request, payload []byte) []*transport.Result {
	return a.Executor().RunEach(ctx, targets, func(transport.Target) transport.Request {
		out := req
		out.Stdin = bytes.NewReader(payload)
		return out
	})
}

// readAll reads the payload of --stdin. A read that fails, even part way
// through, stops the command: a truncated file replayed to every node is
// worse than none.
func readAll(a *app.App) ([]byte, error) {
	if a.In == nil {
		return nil, exitcode.Errorf(exitcode.Usage, "--stdin was given but there is nothing to read")
	}
	type input struct {
		payload []byte
		err     error
	}
	// A read cannot be cancelled, so it is left behind when an interrupt
	// ends the wait; the process is about to exit.
	read := make(chan input, 1)
	go func() {
		payload, err := io.ReadAll(a.In)
		read <- input{payload, err}
	}()
	var got input
	select {
	case got = <-read:
	case <-a.Context().Done():
		return nil, exitcode.Wrap(exitcode.Interrupted,
			fmt.Errorf("reading standard input for --stdin, nothing was sent: %w", a.Context().Err()))
	}
	if got.err != nil {
		return nil, exitcode.Wrap(exitcode.Usage,
			fmt.Errorf("reading standard input for --stdin, nothing was sent: %w", got.err))
	}
	return got.payload, nil
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

	// A node controls its own output, so none of it reaches the terminal
	// as a control sequence.
	out := cmd.OutOrStdout()
	var groups []fanout.Group
	grouped := false
	if dedup {
		var groupErr error
		groups, groupErr = fanout.GroupByOutput(results)
		// Every target has run by now, so what it printed is shown
		// ungrouped rather than lost.
		if grouped = groupErr == nil; !grouped {
			a.Printf("%v; showing the output of each node\n", groupErr)
		}
	}
	if grouped {
		for _, g := range groups {
			if _, err := fmt.Fprintf(out, "%s (%d): %s\n", g.Nodes, g.Nodes.Len(), g.Status); err != nil {
				return err
			}
			// Only the one newline that ends the output is dropped, so that
			// groups that differ in blank lines at the end look different.
			for _, line := range outputLines(strings.TrimSuffix(g.Output, "\n")) {
				if _, err := fmt.Fprintf(out, "  %s\n", output.EscapeText(line)); err != nil {
					return err
				}
			}
		}
	} else {
		for _, res := range results {
			for _, line := range outputLines(strings.TrimRight(res.Stdout, "\n")) {
				if _, err := fmt.Fprintf(out, "%s: %s\n", res.Target.Name, output.EscapeText(line)); err != nil {
					return err
				}
			}
		}
	}
	for _, res := range results {
		if !res.Failed() {
			continue
		}
		line := fanout.Status(res)
		if detail := failureDetail(res); detail != "" {
			line += ": " + detail
		}
		if _, err := fmt.Fprintf(a.Err, "%s: %s\n", res.Target.Name, output.EscapeCell(line)); err != nil {
			return err
		}
	}
	return failureError(results)
}

// outputLines splits output into the lines printed for it. A line that ends
// in CR LF is still one line.
func outputLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}
