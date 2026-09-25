// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/output"
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

// runOnNodes runs one request on a node set and prints a result table of
// what each node answered.
func runOnNodes(a *app.App, ns *nodeset.NodeSet, build func(node string) transport.Request) ([]*transport.Result, error) {
	targets, err := a.NodeTargets(ns)
	if err != nil {
		return nil, err
	}
	results := a.Executor().RunEach(a.Context(), targets, func(t transport.Target) transport.Request {
		return build(t.Name)
	})
	return results, nil
}

// resultsTable renders what each node answered, one row per node.
func resultsTable(results []*transport.Result) *output.Table {
	t := output.NewTable(
		output.Column{Name: "NODE"},
		output.Column{Name: "STATUS"},
		output.Column{Name: "OUTPUT"},
		output.Column{Name: "ERROR", Wide: true},
	)
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
// exits with.
//
// The code says the worst thing that happened, in this order: a target that
// was interrupted exits 130, one that could not be reached 3, and one that
// answered with a failure 1. The errors of the targets are kept, not their
// strings, so that the caller can still tell a cancellation from a failure.
func failureError(results []*transport.Result) error {
	failures := fanout.Failures(results)
	if len(failures) == 0 {
		return nil
	}
	names := nodeset.New()
	code := exitcode.TargetFailed
	var errs []error
	for _, f := range failures {
		_ = names.Add(f.Target.Name)
		if f.Err == nil {
			continue
		}
		errs = append(errs, f.Err)
		switch {
		case errors.Is(f.Err, context.Canceled):
			code = exitcode.Interrupted
		case code != exitcode.Interrupted && exitcode.From(f.Err) == exitcode.Transport:
			code = exitcode.Transport
		}
	}
	return &exitcode.Error{Code: code, Err: &hostFailures{
		message: fmt.Sprintf("%d of %d hosts failed: %s", len(failures), len(results), names),
		errs:    errs,
	}}
}

// hostFailures is the summary of a fan-out that did not succeed everywhere,
// with the error of each target that failed underneath it.
type hostFailures struct {
	message string
	errs    []error
}

func (e *hostFailures) Error() string { return e.message }

func (e *hostFailures) Unwrap() []error { return e.errs }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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
	_ = cmd.RegisterFlagCompletionFunc("context",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			a, err := completionApp(r)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return a.Resolved.Bundle.ContextNames(), cobra.ShellCompDirectiveNoFileComp
		})
	_ = cmd.RegisterFlagCompletionFunc("nodes", completeGroups(r))
}

// completeRoles offers the configured host roles.
func completeRoles(r *root) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		a, err := completionApp(r)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return a.RoleNames(), cobra.ShellCompDirectiveNoFileComp
	}
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
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		a, err := completionApp(r)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
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
		return out, cobra.ShellCompDirectiveNoFileComp
	}
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

// dryRunOrError turns the dry run signal into a clean stop and passes
// anything else on.
func dryRunOrError(err error) error {
	if safety.IsDryRun(err) {
		return nil
	}
	return err
}

// shellQuote quotes one word for a remote shell.
func shellQuote(s string) string { return shellquote.Quote(s) }
