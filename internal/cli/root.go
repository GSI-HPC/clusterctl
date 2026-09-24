// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package cli defines the clusterctl command tree.
//
// The tree is noun then verb, and a short flag means the same thing in every
// command: -n selects nodes, -o selects the output format, -y confirms in
// advance. Anything that changes or destroys something asks first unless -y
// is given, and understands --dry-run.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/internal/version"
)

// root holds the global flags and builds the command context on first use.
type root struct {
	streams app.Streams
	ctx     context.Context

	configFiles []string
	contextName string
	nodes       []string
	format      string
	setValues   []string
	dryRun      bool
	assumeYes   bool
	force       bool
	fanout      int

	// runner replaces the transport; only the tests set it.
	runner transport.Runner
	// agent says the tree runs the commands of an MCP client: there is no
	// default node set, and only the site's hosts may be named.
	agent bool

	cached *app.App
}

// App resolves the configuration once and returns the command context.
func (r *root) App() (*app.App, error) {
	if r.cached != nil {
		return r.cached, nil
	}
	set := map[string]string{}
	for _, assignment := range r.setValues {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok {
			return nil, exitcode.Errorf(exitcode.Usage,
				"--set takes PATH=VALUE, got %q", assignment)
		}
		set[strings.TrimSpace(key)] = value
	}

	nodes, fromFlag, err := r.nodesFromFlagOrEnv()
	if err != nil {
		return nil, err
	}

	a, err := app.New(r.context(), r.streams, app.Options{
		ConfigFiles:   r.configFiles,
		Context:       r.contextName,
		Nodes:         nodes,
		NodesFromFlag: fromFlag,
		Format:        r.format,
		Set:           set,
		DryRun:        r.dryRun,
		AssumeYes:     r.assumeYes,
		Force:         r.force,
		Fanout:        r.fanout,
		Runner:        r.runner,
		SiteHostsOnly: r.agent,
	})
	if err != nil {
		return nil, err
	}
	r.cached = a
	return a, nil
}

// nodesFromFlagOrEnv returns the default node set and whether it came from
// -n rather than from the environment.
//
// An explicit -n is final: CLUSTERCTL_NODES is read only when -n is absent,
// never in place of an empty -n, which is what -n "$(...)" gives when the
// command inside selects nothing. A repeated -n is refused rather than
// having all but the last dropped.
func (r *root) nodesFromFlagOrEnv() (nodes string, fromFlag bool, err error) {
	switch len(r.nodes) {
	case 0:
		// The variable is a default for the administrator's shell. An
		// agent names its nodes or selects none.
		if r.agent {
			return "", false, nil
		}
		return os.Getenv(config.EnvNodes), false, nil
	case 1:
		if strings.TrimSpace(r.nodes[0]) == "" {
			return "", true, exitcode.Errorf(exitcode.Usage,
				"-n was given an empty node set; %s is not used in its place", config.EnvNodes)
		}
		return r.nodes[0], true, nil
	default:
		return "", true, exitcode.Errorf(exitcode.Usage,
			"-n was given %d times; give the whole node set in one -n", len(r.nodes))
	}
}

func (r *root) context() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

// builtRoots remembers which root state belongs to which command tree, so
// that a test can reach the flags of a tree it did not build itself.
var builtRoots = map[*cobra.Command]*root{}

// NewRootCommand builds the command tree.
func NewRootCommand(ctx context.Context, streams app.Streams) *cobra.Command {
	cmd, r := newRoot(ctx, streams)
	builtRoots[cmd] = r
	return cmd
}

// newRoot builds the command tree and returns the state its flags write to.
func newRoot(ctx context.Context, streams app.Streams) (*cobra.Command, *root) {
	r := &root{streams: streams, ctx: ctx}

	cmd := &cobra.Command{
		Use:   "clusterctl",
		Short: "Administer HPC clusters from one binary",
		Long: strings.TrimSpace(`
clusterctl reaches the hosts of an HPC site, selects nodes with ClusterShell
node set syntax, runs commands on them in parallel, drives their service
processors, provisions them and administers Slurm.

Every command reads one layered YAML configuration, so the same binary serves
several clusters and sites. Commands that change something preview what they
are about to do and ask before doing it.`),
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: false},
		Args:              noSubcommand,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	// A flag that cannot be parsed is a usage error, whichever command it
	// was given to: the subcommands ask their parents for this function.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitcode.Wrap(exitcode.Usage, err)
	})

	flags := cmd.PersistentFlags()
	flags.StringSliceVar(&r.configFiles, "config", nil,
		"configuration file or directory to read, repeatable (default: "+config.EnvConfig+" or the search path)")
	flags.StringVar(&r.contextName, "context", "", "context to act on (default: the current one)")
	// An array, not a single string, so that a repeated -n can be told
	// apart from one given once, and an empty -n from none.
	flags.StringArrayVarP(&r.nodes, "nodes", "n", nil,
		"node set to act on, for example 'exe[1-10],@rack:R02' (default: "+config.EnvNodes+")")
	flags.StringVarP(&r.format, "output", "o", "table",
		"output format: "+strings.Join(output.Formats(), ", "))
	flags.StringArrayVar(&r.setValues, "set", nil, "override one configuration value as PATH=VALUE, repeatable")
	flags.BoolVar(&r.dryRun, "dry-run", false, "report what would be done and change nothing")
	flags.BoolVarP(&r.assumeYes, "yes", "y", false, "answer the confirmation prompts with yes")
	flags.BoolVar(&r.force, "force", false, "allow protected hosts, and nodes the inventory does not know, to be touched")
	flags.IntVar(&r.fanout, "fanout", 0, "how many hosts to work on at once (default: from the configuration)")

	registerCompletions(cmd, r)

	cmd.AddCommand(
		newConfigCommand(r),
		newLoginCommand(r),
		newExecCommand(r),
		newCopyCommand(r),
		newNodeCommand(r),
		newHostkeyCommand(r),
		newTunnelCommand(r),
		newBMCCommand(r),
		newPDUCommand(r),
		newFabricCommand(r),
		newHCACommand(r),
		newBootCommand(r),
		newDHCPCommand(r),
		newSecretsCommand(r),
		newCincCommand(r),
		newProvisionCommand(r),
		newDNSCommand(r),
		newSlurmCommand(r),
		newDoctorCommand(r),
		newMCPCommand(r),
		newVersionCommand(r),
	)
	annotateEffects(cmd)
	usageArgs(cmd)
	return cmd, r
}

// noSubcommand is the argument check of a command that only holds
// subcommands. A word that names none of them is refused, rather than
// handed to the command, which would print its help and succeed: a script
// that misspelt "drain" would go on to its next step.
func noSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	suggestion := ""
	if found := cmd.SuggestionsFor(args[0]); len(found) > 0 {
		suggestion = "; did you mean " + strings.Join(found, " or ") + "?"
	}
	return exitcode.Errorf(exitcode.Usage, "unknown command %q for %q%s",
		args[0], cmd.CommandPath(), suggestion)
}

// usageArgs makes every argument check in the tree report a usage error.
// cobra's own checks, such as cobra.ExactArgs, return plain errors, which
// would otherwise exit 1, the code for a target that failed.
func usageArgs(cmd *cobra.Command) {
	if check := cmd.Args; check != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			err := check(c, args)
			var coded *exitcode.Error
			if err == nil || errors.As(err, &coded) {
				return err
			}
			return exitcode.Wrap(exitcode.Usage, err)
		}
	}
	for _, sub := range cmd.Commands() {
		usageArgs(sub)
	}
}

// Execute runs the command tree and returns the process exit code.
func Execute(ctx context.Context) int {
	streams := app.DefaultStreams()
	cmd := NewRootCommand(ctx, streams)
	cmd.SetIn(streams.In)
	cmd.SetOut(streams.Out)
	cmd.SetErr(streams.Err)

	return execute(ctx, cmd, streams)
}

// execute runs a built command tree and returns the process exit code.
func execute(ctx context.Context, cmd *cobra.Command, streams app.Streams) int {
	if err := cmd.ExecuteContext(ctx); err != nil {
		return report(streams, err)
	}
	return exitcode.OK
}

// report prints an error and returns the exit code it asks for. A dry run
// that stopped on purpose is not an error.
func report(streams app.Streams, err error) int {
	if safety.IsDryRun(err) {
		return exitcode.OK
	}
	if errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintln(streams.Err, "clusterctl: interrupted")
		return exitcode.Interrupted
	}
	// The exit code is what a caller acts on; a message that cannot be
	// written changes nothing about it.
	_, _ = fmt.Fprintf(streams.Err, "clusterctl: %v\n", err)
	return exitcode.From(err)
}

func newVersionCommand(r *root) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build provenance of this binary",
		Long: strings.TrimSpace(`
Print the version, the revision it was built from and the toolchain that
built it. No version number is stored in the source tree: a release build
takes it from the signed git tag.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := version.Get()
			format, err := output.ParseFormat(r.format)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			if format.Kind == output.FormatTable || format.Kind == output.FormatWide {
				return say(cmd, "%s\n", info)
			}
			return format.WriteContext(cmd.Context(), cmd.OutOrStdout(), output.Result{Object: info})
		},
	}
}
