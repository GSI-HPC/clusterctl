// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/output"
)

func newConfigCommand(r *root) *cobra.Command {
	return group("config", "Start, inspect and check the configuration", `
The configuration is read from several documents and merged in layers:
built-in defaults, the site, the cluster, the workstation, the context, the
environment and finally the flags. Each layer wins over the ones before it.

These commands show what the merge produced and where each value came from.
"clusterctl config init" writes a first configuration to start from.`,
		newConfigInitCommand(r),
		newConfigViewCommand(r),
		newConfigValidateCommand(r),
		newConfigContextsCommand(r),
		newConfigUseContextCommand(r),
		newConfigExplainCommand(r),
		newConfigSchemaCommand(r),
	)
}

func newConfigInitCommand(r *root) *cobra.Command {
	opts := config.ScaffoldDefaults()

	cmd := leaf("init [DIR]", "Write a first configuration to fill in", `
Write the least configuration that resolves, one document to a file: a
Config with one context, a Site with a login node, a Cluster and an empty
NodeInventory. The comments in each file say what to fill in; then check the
result with "clusterctl config validate" and "clusterctl doctor".

Without DIR the files are written where configuration is read from: the
directory --config or CLUSTERCTL_CONFIG names, or else your own configuration
directory, usually ~/.config/clusterctl. When either names several places,
name the one to write to, and so when /etc/clusterctl, which is read together
with your own directory, holds configuration already. Directories that are
missing are created.

The directory has to be empty. One that holds anything, hidden files
included, is refused rather than added to: this command writes nothing next
to files it did not write, and overwrites nothing.

  clusterctl config init
  clusterctl config init --site lab --cluster alpha --domain hpc.example.org --user alice_adm
  clusterctl config init ./site-config --dry-run`,
		cobra.MaximumNArgs(1),
		func(cmd *cobra.Command, args []string) error {
			format, err := output.ParseFormat(r.format)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			dir, err := initDir(r, args)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			files, err := config.Scaffold(opts)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			if err := refuseNonEmpty(dir); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}

			t := output.NewTable(output.Cols("FILE", "KIND", "NAME")...)
			for _, f := range files {
				t.Add(filepath.Join(dir, f.Name), f.Kind, f.DocName)
			}
			if r.dryRun {
				t.Caption = fmt.Sprintf("%d files would be written; nothing was", len(files))
				return format.WriteContext(cmd.Context(), cmd.OutOrStdout(), output.Result{Table: t})
			}
			// Nothing was sent anywhere: a directory that cannot be
			// written is a mistake in what was asked, not a failed target.
			if err := writeScaffold(dir, files); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			t.Caption = initNextSteps(r, dir, len(files))
			return format.WriteContext(cmd.Context(), cmd.OutOrStdout(), output.Result{Table: t})
		})

	flags := cmd.Flags()
	flags.StringVar(&opts.Site, "site", opts.Site, "name of the Site document")
	flags.StringVar(&opts.Cluster, "cluster", opts.Cluster, "name of the Cluster document and of the context that acts on it")
	flags.StringVar(&opts.Domain, "domain", opts.Domain, "DNS domain of the cluster nodes")
	flags.StringVar(&opts.Login, "login", "", "host name of the login node (default: login in the domain)")
	flags.StringVar(&opts.User, "user", "", "remote account to log in as (default: left to ssh)")
	cmd.ValidArgsFunction = func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveFilterDirs
	}
	return cmd
}

// initDir is where config init writes: the directory it was given, else the
// one place --config or CLUSTERCTL_CONFIG names, else the administrator's own
// configuration directory. Writing where configuration is read from means
// the next command reads what was written.
func initDir(r *root, args []string) (string, error) {
	if len(args) == 1 {
		if strings.TrimSpace(args[0]) == "" {
			return "", fmt.Errorf("the directory to write to is empty")
		}
		return filepath.Abs(config.ExpandPath(args[0], ""))
	}
	source, places := "--config", nonBlank(r.configFiles)
	if len(places) == 0 {
		source, places = config.EnvConfig, nonBlank(config.EnvEntries(nil))
	}
	switch len(places) {
	case 0:
	case 1:
		return filepath.Abs(config.ExpandPath(places[0], ""))
	default:
		// A complete configuration written into one layer of several
		// would change what the others resolve to.
		return "", fmt.Errorf("%s names %d places to read configuration from (%s); name the one to write to: clusterctl config init DIR",
			source, len(places), strings.Join(places, ", "))
	}
	dir, err := config.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no configuration directory is known for this user (%w); name one: clusterctl config init DIR", err)
	}
	// The search path reads the user's directory together with the ones
	// before it, /etc/clusterctl first. A complete configuration written
	// next to a team's would replace its documents of the same name or
	// move the current context to the new site, and either way its
	// protected hosts would stop applying.
	for _, other := range configDirs() {
		if other == dir {
			continue
		}
		files, err := config.ExpandSearchDirs([]string{other})
		if err != nil {
			return "", fmt.Errorf("cannot tell whether %s holds configuration (%w); name the directory to write to: clusterctl config init DIR", other, err)
		}
		if len(files) > 0 {
			return "", fmt.Errorf("%s holds configuration already, which is read together with %s; "+
				"a second one there would change what it resolves to. "+
				"Name the directory to write to: clusterctl config init DIR",
				other, dir)
		}
	}
	return dir, nil
}

// configDirs are the directories the search path reads; the tests replace
// it to stand in for /etc/clusterctl.
var configDirs = config.ConfigDirs

// nonBlank returns the entries that are not blank, trimmed.
func nonBlank(entries []string) []string {
	var out []string
	for _, entry := range entries {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// refuseNonEmpty stops config init from writing into a directory that holds
// anything at all. Whatever is there was put there by someone else, and
// documents written next to it could change what it resolves to.
//
// Nor does it write into a directory someone other than this user or root
// owns or can write, or create one inside such a directory: whoever can
// write it can add to the configuration clusterctl is then pointed at.
func refuseNonEmpty(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuseUntrustedParent(dir)
	case err != nil:
		return err
	case !info.IsDir():
		return exitcode.Errorf(exitcode.Usage, "%s is not a directory", dir)
	}
	if err := fileutil.CheckTrusted(dir); err != nil {
		return exitcode.Wrap(exitcode.Usage, fmt.Errorf("config init writes only into a directory of its own: %w", err))
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	const shown = 3
	names := make([]string, 0, shown+1)
	for _, item := range items[:min(len(items), shown)] {
		names = append(names, item.Name())
	}
	if len(items) > shown {
		names = append(names, fmt.Sprintf("and %d more", len(items)-shown))
	}
	return exitcode.Errorf(exitcode.Usage,
		"%s is not empty (%s); config init writes only into an empty directory: name a new one, clusterctl config init DIR",
		dir, strings.Join(names, ", "))
}

// refuseUntrustedParent checks the closest directory that exists above a
// directory config init is going to create.
func refuseUntrustedParent(dir string) error {
	for {
		parent := filepath.Dir(dir)
		_, err := os.Stat(parent)
		switch {
		case err == nil:
			if err := fileutil.CheckTrustedParent(dir); err != nil {
				return exitcode.Wrap(exitcode.Usage, fmt.Errorf("config init creates a directory only where others cannot replace it: %w", err))
			}
			return nil
		case !errors.Is(err, fs.ErrNotExist) || parent == dir:
			return err
		}
		dir = parent
	}
}

// writeScaffold writes the files of a new configuration, all of them or none:
// when one cannot be written, the ones written before it are taken away.
func writeScaffold(dir string, files []config.ScaffoldFile) (err error) {
	// Every administrator of the site reads the site documents, so the
	// directory is not made private the way the state directory is.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var written []string
	defer func() {
		if err != nil {
			for _, path := range written {
				_ = os.Remove(path)
			}
		}
	}()
	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		if err = fileutil.WriteNew(path, f.Data, 0o644); err != nil {
			return err
		}
		written = append(written, path)
	}
	return nil
}

// initNextSteps says what to do with the files config init wrote, including
// how to have them read when they are not where clusterctl looks.
func initNextSteps(r *root, dir string, count int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d files written. The comments in them say what to fill in; then check the result:\n", count)
	if !searched(r, dir) {
		fmt.Fprintf(&b, "  export %s=%s    # clusterctl does not read this directory otherwise\n",
			config.EnvConfig, shellQuote(dir))
	}
	b.WriteString("  clusterctl config validate\n  clusterctl doctor")
	return b.String()
}

// searched reports whether configuration in dir is read without being named
// again: dir is given to --config, listed in CLUSTERCTL_CONFIG, or one of the
// directories searched when neither is set.
func searched(r *root, dir string) bool {
	entries := r.configFiles
	if len(entries) == 0 {
		entries = config.SearchEntries(nil)
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if abs, err := filepath.Abs(config.ExpandPath(entry, "")); err == nil && abs == dir {
			return true
		}
	}
	return false
}

func newConfigViewCommand(r *root) *cobra.Command {
	var showSources bool

	cmd := leaf("view", "Print the merged configuration", `
Print the configuration the current context resolves to.

With --show-sources each value is listed with the layer that set it and, for
a value that came from a file, the line it was written on.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
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
				t.Add(path, origin.Layer, where, config.FormatValue(a.Resolved.Tree.Value(path)))
			}
			t.Caption = fmt.Sprintf("context %s, cluster %s, site %s",
				a.Resolved.Context.Name, a.Resolved.ClusterName, a.Resolved.SiteName)
			return a.Print(output.Result{Table: t})
		}))
	cmd.Flags().BoolVar(&showSources, "show-sources", false, "list each value with the layer and line that set it")
	return cmd
}

func newConfigValidateCommand(r *root) *cobra.Command {
	return leaf("validate", "Check the configuration files", `
Read every configuration document, validate it against the schema of its kind
and resolve the current context. Every cluster, not only the current one, is
checked to read only its own site's inventories. Problems are reported with
the file, the line and the path they were found at.

This is what to run after editing the configuration and in a pipeline that
publishes it.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			// Protected host entries that name a group are otherwise only
			// resolved when a command is about to change something.
			if _, err := a.Gate.Protected(); err != nil {
				return err
			}
			// Every cluster, not only the current one, has to say which
			// nodes it reads.
			if err := a.CheckClusters(); err != nil {
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
		}))
}

func newConfigContextsCommand(r *root) *cobra.Command {
	return leaf("contexts", "List the configured contexts", `
List the contexts this installation knows and mark the current one.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			t := output.NewTable(output.Cols("CURRENT", "NAME", "CLUSTER", "USER")...)
			for _, ctx := range a.Resolved.Bundle.Config.Contexts {
				marker := ""
				if ctx.Name == a.Resolved.Context.Name {
					marker = "*"
				}
				t.Add(marker, ctx.Name, ctx.Cluster, ctx.User)
			}
			return a.Print(output.Result{Table: t})
		}))
}

func newConfigUseContextCommand(r *root) *cobra.Command {
	cmd := leaf("use-context NAME", "Print how to make a context current", `
Report what to change to make a context current.

The configuration is a file the team keeps under version control, so this
command does not edit it. It prints the one line to change, and the
environment variable that selects a context for a single shell.`,
		cobra.ExactArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			name := args[0]
			if _, err := a.Resolved.Bundle.Context(name); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			// Each line is meant to be pasted, so the name is quoted
			// for where it goes.
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"For this shell:\n  export %s=%s\n\n"+
					"For one command:\n  clusterctl --context %s ...\n\n"+
					"Permanently, in the Config document:\n  currentContext: %s\n",
				config.EnvContext, shellQuote(name), shellQuote(name), config.QuoteYAML(name))
			return err
		}))
	cmd.ValidArgsFunction = completeContexts(r)
	return cmd
}

func newConfigExplainCommand(r *root) *cobra.Command {
	return leaf("explain PATH", "Show where one configuration value came from", `
Print the value at a dotted path, the layer that set it and the line it was
written on.

  clusterctl config explain fanout.max
  clusterctl config explain bmc.ipmi.backend`,
		cobra.ExactArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			path := args[0]
			origin, ok := a.Resolved.Tree.Origin(path)
			if !ok {
				return exitcode.Errorf(exitcode.Usage,
					"nothing is set at %q; %s", path, nearestPaths(a.Resolved.Tree.Paths(), path))
			}
			value := a.Resolved.Tree.Value(path)
			return a.Print(output.Result{Object: map[string]any{
				"path":   path,
				"value":  value,
				"layer":  origin.Layer,
				"file":   origin.File,
				"line":   origin.Line,
				"column": origin.Column,
			}, Table: explainTable(path, value, origin)})
		}))
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
