// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
)

func newConfigCommand(r *root) *cobra.Command {
	return group("config", "Inspect and check the configuration", `
The configuration is read from several documents and merged in layers:
built-in defaults, the site, the cluster, the workstation, the context, the
environment and finally the flags. Each layer wins over the ones before it.

These commands show what the merge produced and where each value came from.`,
		newConfigViewCommand(r),
		newConfigValidateCommand(r),
		newConfigContextsCommand(r),
		newConfigUseContextCommand(r),
		newConfigExplainCommand(r),
		newConfigSchemaCommand(r),
	)
}

func newConfigViewCommand(r *root) *cobra.Command {
	var showSources bool

	cmd := leaf("view", "Print the merged configuration", `
Print the configuration the current context resolves to.

With --show-sources each value is listed with the layer that set it and, for
a value that came from a file, the line it was written on.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			if !showSources {
				return a.Print(output.Result{Object: a.Spec})
			}

			// The value comes last and unpadded: a naming rule or a list of
			// secrets is long, and a wide VALUE column would push the layer
			// off the screen for every other row.
			t := output.NewTable(
				output.Column{Name: "PATH"},
				output.Column{Name: "LAYER"},
				output.Column{Name: "SOURCE", Wide: true},
				output.Column{Name: "VALUE"},
			)
			origins := a.Resolved.Tree.Origins()
			paths := a.Resolved.Tree.Paths()
			for _, path := range paths {
				origin := origins[path]
				where := origin.File
				if where != "" && origin.Line > 0 {
					where = fmt.Sprintf("%s:%d:%d", origin.File, origin.Line, origin.Column)
				}
				t.Add(path, origin.Layer, where, config.FormatValue(lookupPath(a.Resolved.Tree.Data(), path)))
			}
			t.Caption = fmt.Sprintf("context %s, cluster %s, site %s",
				a.Resolved.Context.Name, a.Resolved.ClusterName, a.Resolved.SiteName)
			return a.Print(output.Result{Table: t})
		})
	cmd.Flags().BoolVar(&showSources, "show-sources", false, "list each value with the layer and line that set it")
	return cmd
}

func newConfigValidateCommand(r *root) *cobra.Command {
	return leaf("validate", "Check the configuration files", `
Read every configuration document, validate it against the schema of its kind
and resolve the current context. Problems are reported with the file, the line
and the path they were found at.

This is what to run after editing the configuration and in a pipeline that
publishes it.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("FILE", "KIND", "NAME")...)
			for _, doc := range a.Resolved.Bundle.Documents {
				name := ""
				if meta, ok := doc.Data["metadata"].(map[string]any); ok {
					name, _ = meta["name"].(string)
				}
				t.Add(doc.File, doc.Kind, name)
			}
			t.Caption = fmt.Sprintf("%d documents are valid; context %s resolves",
				len(a.Resolved.Bundle.Documents), a.Resolved.Context.Name)
			return a.Print(output.Result{Table: t})
		})
}

func newConfigContextsCommand(r *root) *cobra.Command {
	return leaf("contexts", "List the configured contexts", `
List the contexts this installation knows and mark the current one.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("CURRENT", "NAME", "CLUSTER", "USER")...)
			for _, ctx := range a.Resolved.Bundle.Config.Contexts {
				marker := ""
				if ctx.Name == a.Resolved.Context.Name {
					marker = "*"
				}
				t.Add(marker, ctx.Name, ctx.Cluster, ctx.User)
			}
			return a.Print(output.Result{Table: t})
		})
}

func newConfigUseContextCommand(r *root) *cobra.Command {
	cmd := leaf("use-context NAME", "Print how to make a context current", `
Report what to change to make a context current.

The configuration is a file the team keeps under version control, so this
command does not edit it. It prints the one line to change, and the
environment variable that selects a context for a single shell.`,
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			name := args[0]
			if _, err := a.Resolved.Bundle.Context(name); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"For this shell:\n  export %s=%s\n\n"+
					"For one command:\n  clusterctl --context %s ...\n\n"+
					"Permanently, in the Config document:\n  currentContext: %s\n",
				config.EnvContext, name, name, name)
			return err
		})
	cmd.ValidArgsFunction = func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		a, err := r.App()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return a.Resolved.Bundle.ContextNames(), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func newConfigExplainCommand(r *root) *cobra.Command {
	return leaf("explain PATH", "Show where one configuration value came from", `
Print the value at a dotted path, the layer that set it and the line it was
written on.

  clusterctl config explain fanout.max
  clusterctl config explain bmc.ipmi.passwordTransport`,
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			path := args[0]
			origin, ok := a.Resolved.Tree.Origin(path)
			if !ok {
				return exitcode.Errorf(exitcode.Usage,
					"nothing is set at %q; %s", path, nearestPaths(a.Resolved.Tree.Paths(), path))
			}
			value := lookupPath(a.Resolved.Tree.Data(), path)
			return a.Print(output.Result{Object: map[string]any{
				"path":   path,
				"value":  value,
				"layer":  origin.Layer,
				"file":   origin.File,
				"line":   origin.Line,
				"column": origin.Column,
			}, Table: explainTable(path, value, origin)})
		})
}

func explainTable(path string, value any, origin config.Origin) *output.Table {
	t := output.NewTable(output.Cols("FIELD", "VALUE")...)
	t.Add("path", path)
	t.Add("value", config.FormatValue(value))
	t.Add("layer", origin.Layer)
	if origin.File != "" {
		where := origin.File
		if origin.Line > 0 {
			where = fmt.Sprintf("%s:%d:%d", origin.File, origin.Line, origin.Column)
		}
		t.Add("source", where)
	}
	return t
}

func newConfigSchemaCommand(r *root) *cobra.Command {
	cmd := leaf("schema [KIND]", "Print the JSON Schema of a configuration kind", `
Print the JSON Schema an editor needs to check and complete a configuration
file. Point the YAML language server at it with a comment at the top of the
file:

  # yaml-language-server: $schema=./site.schema.json

Without an argument every kind is printed as one object keyed by kind.`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				data, err := config.SchemaJSON(args[0])
				if err != nil {
					return exitcode.Wrap(exitcode.Usage, err)
				}
				return say(cmd, "%s\n", data)
			}
			for _, kind := range config.SchemaKinds() {
				data, err := config.SchemaJSON(kind)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(out, "// %s\n%s\n", kind, data); err != nil {
					return err
				}
			}
			return nil
		})
	cmd.ValidArgsFunction = fixed(v1alpha1.Kinds()...)
	return cmd
}

// lookupPath reads a dotted path out of the merged tree.
func lookupPath(data map[string]any, path string) any {
	var current any = data
	for _, part := range strings.Split(path, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	return current
}

// nearestPaths suggests the configured paths closest to one that is not set.
func nearestPaths(paths []string, want string) string {
	var close []string
	for _, p := range paths {
		if strings.HasPrefix(p, want) || strings.HasPrefix(want, p) {
			close = append(close, p)
		}
	}
	if len(close) == 0 {
		return "run \"clusterctl config view --show-sources\" to see what is set"
	}
	sort.Strings(close)
	if len(close) > 8 {
		close = close[:8]
	}
	return "did you mean one of: " + strings.Join(close, ", ")
}
