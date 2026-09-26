// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// group builds a command that only holds subcommands.
func group(use, short, long string, subs ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  strings.TrimSpace(long),
		Args:  noSubcommand,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	cmd.AddCommand(subs...)
	return cmd
}

// leaf builds a command that does something.
func leaf(use, short, long string, args cobra.PositionalArgs, run func(*cobra.Command, []string) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Long:  strings.TrimSpace(long),
		Args:  args,
		RunE:  run,
	}
}

// say writes to a command's output.
//
// The error is returned rather than discarded: a closed pipe, which is what
// happens when output is piped into head, should stop the command instead of
// leaving it running against nothing.
func say(cmd *cobra.Command, format string, args ...any) error {
	_, err := fmt.Fprintf(cmd.OutOrStdout(), format, args...)
	return err
}

// session connects the terminal to a command on a remote host, or to a
// shell there when req has none. A dry run prints the ssh command line
// instead.
func session(ctx context.Context, a *app.App, cmd *cobra.Command, target transport.Target, req transport.Request) error {
	if a.DryRun() {
		line, err := a.SSH.Args(target, req)
		if err != nil {
			return err
		}
		return say(cmd, "%s\n", strings.Join(line, " "))
	}
	return a.SSH.Interactive(ctx, target, req)
}

// printLines prints what a host printed: as it came, escaped, in the table
// formats, and as a list of its lines in the others, so that -o json and a
// jq program read it too.
func printLines(a *app.App, cmd *cobra.Command, text string) error {
	if k := a.Format.Kind; k == output.FormatTable || k == output.FormatWide {
		return say(cmd, "%s\n", output.EscapeText(text))
	}
	lines := []string{}
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	return a.Print(output.Result{Object: lines})
}

// selection resolves the node set a command acts on, from its argument or
// from -n, or else from CLUSTERCTL_NODES. An argument together with -n is a
// usage error, as is an empty selection.
func selection(a *app.App, args []string) (*nodeset.NodeSet, error) {
	expr := ""
	if len(args) > 0 {
		expr = strings.Join(args, ",")
	}
	return a.Select(expr)
}

// oneNode resolves the one node a command's NODE argument names, the way a
// selection is resolved: a name that is not a host name is refused, and any
// spelling of a machine, in other case or padding or as its host name, is
// the name the inventory uses for it.
func oneNode(a *app.App, arg string) (string, error) {
	if strings.TrimSpace(arg) == "" {
		return "", exitcode.Errorf(exitcode.Usage, "no node was named")
	}
	ns, err := a.Select(arg)
	if err != nil {
		return "", err
	}
	if ns.Len() != 1 {
		return "", exitcode.Errorf(exitcode.Usage, "%q names %d nodes; name one", arg, ns.Len())
	}
	return ns.Expand()[0], nil
}

// runOnNodes runs one request on a node set and returns what each node
// answered. The request gets no terminal, and the configured command
// timeout unless it sets one.
func runOnNodes(ctx context.Context, a *app.App, ns *nodeset.NodeSet, req transport.Request) ([]*transport.Result, error) {
	targets, err := a.NodeTargets(ns)
	if err != nil {
		return nil, err
	}
	return a.Executor().Run(ctx, targets, a.Collect(req)), nil
}

// resultsTable renders what each node answered, one row per node.
func resultsTable(results []*transport.Result) *output.Table {
	t := output.NewTable(output.Cols("NODE", "STATUS", "OUTPUT", "ERROR").Wide("ERROR")...)
	for _, r := range results {
		status, detail := fanout.Status(r), failureDetail(r)
		t.Add(r.Target.Name, status, firstLine(r.Output()), detail)
	}
	return t
}

// failureDetail is what a failed target said about its failure: the last
// line of its standard error, or the error of the transport when it said
// nothing. A command that exited non-zero without a word is described by its
// status alone.
func failureDetail(r *transport.Result) string {
	if !r.Failed() {
		return ""
	}
	if line := strings.TrimSpace(lastNonEmpty(r.Stderr)); line != "" {
		return line
	}
	if r.Err != nil && (r.ExitCode <= 0 || exitcode.From(r.Err) == exitcode.Transport) {
		return r.Err.Error()
	}
	return ""
}

// failureError turns the failures of a fan-out into the error the process
// exits with, as fanout.FailureError sums them up: "k of n hosts failed",
// with the code exitcode.Worst gives, and the errors of the targets kept
// underneath, so that the caller can still tell a cancellation from a
// failure.
func failureError(results []*transport.Result) error {
	return fanout.FailureError(results)
}

// hostFailures is the summary of a fan-out that did not succeed everywhere,
// with the error of each target that failed underneath it.
type hostFailures struct {
	message string
	errs    []error
	code    int
}

func (e *hostFailures) Error() string { return e.message }

func (e *hostFailures) Unwrap() []error { return e.errs }

// ProgressClass says why the hosts failed as the exit code does.
func (e *hostFailures) ProgressClass() progress.Class { return progress.CodeClass(e.code) }

func firstLine(s string) string {
	before, _, _ := strings.Cut(s, "\n")
	return before
}

func lastNonEmpty(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// completionApp builds the command context for a completion.
//
// Cobra parses the flags of the command line being completed twice, and a
// flag that collects its values, such as --config, -n or --set, then holds
// each of them twice. The configuration would be read twice, which is
// refused, and -n would look repeated, so completion offered nothing whenever
// the command line carried --config or -n. A value given twice means the same
// as once for completion, so the repeats are dropped first.
func completionApp(r *root) (*app.App, error) {
	r.configFiles = uniqueWords(r.configFiles)
	r.nodes = uniqueWords(r.nodes)
	r.setValues = uniqueWords(r.setValues)
	return r.App()
}

// uniqueWords drops the repeats of a list, keeping the first of each in its
// place.
func uniqueWords(words []string) []string {
	seen := make(map[string]bool, len(words))
	out := words[:0:0]
	for _, w := range words {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// registerCompletions wires shell completion for the flags whose values come
// from the configuration.
func registerCompletions(cmd *cobra.Command, r *root) {
	_ = cmd.RegisterFlagCompletionFunc("output",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return output.Formats(), cobra.ShellCompDirectiveNoFileComp
		})
	_ = cmd.RegisterFlagCompletionFunc("context", completeContexts(r))
	_ = cmd.RegisterFlagCompletionFunc("nodes", completeGroups(r))
}

// complete offers the words names returns for the command context, and none
// when the configuration cannot be read.
func complete(r *root, names func(*app.App) []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		a, err := completionApp(r)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return names(a), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeContexts offers the configured contexts.
func completeContexts(r *root) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return complete(r, func(a *app.App) []string { return a.Resolved.Bundle.ContextNames() })
}

// completeRoles offers the configured host roles.
func completeRoles(r *root) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return complete(r, (*app.App).RoleNames)
}

// completeGroups offers the configured groups, which is what a node set
// expression most often starts with.
//
// Only the groups that can be listed without running anything are offered:
// the tables of the configuration and the attributes of the inventory. A
// source that runs a command, such as sinfo on the Slurm host, would open an
// ssh connection on every Tab, freeze the shell while a slow host answers and
// could prompt for a passphrase in the middle of the command line, so its
// groups are left out and have to be typed.
func completeGroups(r *root) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return complete(r, func(a *app.App) []string {
		var out []string
		for _, source := range a.Groups.Sources() {
			if a.Spec.Groups.Sources[source].Exec != nil {
				continue
			}
			names, err := a.Groups.List(source)
			if err != nil {
				continue
			}
			for _, name := range names {
				out = append(out, "@"+source+":"+name)
			}
		}
		sort.Strings(out)
		return out
	})
}

// fixed offers a fixed list of words for completion.
func fixed(words ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return words, cobra.ShellCompDirectiveNoFileComp
	}
}

// safetyAction builds the description the confirmation gate previews.
func safetyAction(verb string, targets *nodeset.NodeSet, detail string) safety.Action {
	return safety.Action{Verb: verb, Targets: targets, Detail: detail}
}

// shellQuote quotes one word for a remote shell.
func shellQuote(s string) string { return shellquote.Quote(s) }
