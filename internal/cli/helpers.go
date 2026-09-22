// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
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

// selection resolves the node set a command acts on, from its argument or
// from -n.
func selection(a *app.App, args []string) (*nodeset.NodeSet, error) {
	expr := ""
	if len(args) > 0 {
		expr = strings.Join(args, ",")
	}
	return a.Select(expr)
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
		status := "ok"
		if r.Failed() {
			status = fmt.Sprintf("exit %d", r.ExitCode)
		}
		detail := ""
		if r.Err != nil {
			detail = r.Err.Error()
		} else if r.Failed() {
			detail = strings.TrimSpace(lastNonEmpty(r.Stderr))
		}
		t.Add(r.Target.Name, status, firstLine(r.Output()), detail)
	}
	return t
}

// failureError turns the failures of a fan-out into the error the process
// exits with, which is a target failure rather than a usage or transport
// problem.
func failureError(results []*transport.Result) error {
	failures := fanout.Failures(results)
	if len(failures) == 0 {
		return nil
	}
	names := nodeset.New()
	for _, f := range failures {
		_ = names.Add(f.Target.Name)
	}
	return exitcode.Errorf(exitcode.TargetFailed, "%d of %d hosts failed: %s",
		len(failures), len(results), names)
}

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

// registerCompletions wires shell completion for the flags whose values come
// from the configuration.
func registerCompletions(cmd *cobra.Command, r *root) {
	_ = cmd.RegisterFlagCompletionFunc("output",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return output.Formats(), cobra.ShellCompDirectiveNoFileComp
		})
	_ = cmd.RegisterFlagCompletionFunc("context",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			a, err := r.App()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return a.Resolved.Bundle.ContextNames(), cobra.ShellCompDirectiveNoFileComp
		})
	_ = cmd.RegisterFlagCompletionFunc("nodes",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			a, err := r.App()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []string
			for _, source := range a.Groups.Sources() {
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
		})
}

// completeRoles offers the configured host roles.
func completeRoles(r *root) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		a, err := r.App()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return a.RoleNames(), cobra.ShellCompDirectiveNoFileComp
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
