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
	"github.com/GSI-HPC/clusterctl/internal/slurm"
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
	// fanoutGiven tells a --fanout that was given from one that was not.
	fanoutGiven func() bool
	progress    string
	// progressGiven tells a --progress that was given from one that was
	// not, which leaves the choice to the environment.
	progressGiven func() bool

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
	set, err := r.overrides()
	if err != nil {
		return nil, err
	}

	// Zero is what an absent --fanout reads as, so a given one below one
	// would otherwise be dropped without a word.
	if r.fanoutGiven != nil && r.fanoutGiven() && r.fanout < 1 {
		return nil, exitcode.Errorf(exitcode.Usage, "--fanout is %d; it must be at least 1", r.fanout)
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

// run builds the function of a command that works in the command context,
// resolving the context first.
func (r *root) run(fn func(a *app.App, cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		a, err := r.App()
		if err != nil {
			return err
		}
		return fn(a, cmd, args)
	}
}

// runSlurm is run for a command that asks the Slurm clients of the cluster.
func (r *root) runSlurm(fn func(a *app.App, c *slurm.Client, cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
		c, err := a.Slurm()
		if err != nil {
			return err
		}
		return fn(a, c, cmd, args)
	})
}

// overrides reads the --set flags into the values they override.
func (r *root) overrides() (map[string]string, error) {
	set := map[string]string{}
	for _, assignment := range r.setValues {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok {
			return nil, exitcode.Errorf(exitcode.Usage, "--set takes PATH=VALUE, got %q", assignment)
		}
		set[strings.TrimSpace(key)] = value
	}
	return set, nil
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
	flags.IntVar(&r.fanout, "fanout", 0,
		"how many hosts to work on at once, at least 1; caps the service processors and the names asked at once too, which fanout.max and CLUSTERCTL_FANOUT do not (default: from the configuration)")
	r.fanoutGiven = func() bool { return flags.Changed("fanout") }
	flags.StringVar(&r.progress, "progress", "",
		"how to show the progress of a command on standard error: "+strings.Join(progressModes, ", ")+
			" (default: "+config.EnvProgress+", else auto, a live tree when standard error is a terminal; plain writes lines for a log)")
	r.progressGiven = func() bool { return flags.Changed("progress") }

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
	builtins(cmd, streams)
	annotateEffects(cmd)
	usageArgs(cmd)
	traceLeaves(cmd, r)
	return cmd, r
}

// builtins adds cobra's help and completion commands to the tree now, rather
// than when it runs, so that the argument checks below cover them: cobra's
// own completion printed its help and succeeded for a shell it does not know,
// and its help printed the root help and succeeded for a topic that names no
// command.
func builtins(cmd *cobra.Command, streams app.Streams) {
	// The completion scripts are written to the output the root has when the
	// command is made, not when it runs.
	cmd.SetOut(streams.Out)
	cmd.InitDefaultHelpCmd()
	cmd.InitDefaultCompletionCmd()
	for _, sub := range cmd.Commands() {
		switch sub.Name() {
		case "help":
			sub.Args = helpTopic
		case "completion":
			// A group, like every other: without a function of its own
			// cobra prints its help before any argument is looked at.
			sub.Args = noSubcommand
			sub.RunE = func(c *cobra.Command, _ []string) error {
				return c.Help()
			}
		}
	}
}

// noSubcommand is the argument check of a command that only holds
// subcommands. A word that names none of them is refused, rather than
// handed to the command, which would print its help and succeed: a script
// that misspelt "drain" would go on to its next step.
func noSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return exitcode.Errorf(exitcode.Usage, "unknown command %q for %q%s",
		args[0], cmd.CommandPath(), suggest(cmd, args[0]))
}

// helpTopic is the argument check of the help command. The topic has to name
// a command, all of it: cobra's help prints the root help for a topic it
// cannot find, and the help of the nearest command for one with words left
// over, and succeeds either way.
func helpTopic(cmd *cobra.Command, args []string) error {
	found, rest, err := cmd.Root().Find(args)
	if err == nil && found != nil && len(rest) == 0 {
		return nil
	}
	topic := strings.Join(args, " ")
	if err != nil || found == nil || len(rest) == 0 {
		return exitcode.Errorf(exitcode.Usage, "unknown help topic %q", topic)
	}
	return exitcode.Errorf(exitcode.Usage, "unknown help topic %q: unknown command %q for %q%s",
		topic, rest[0], found.CommandPath(), suggest(found, rest[0]))
}

// suggest names the subcommands of cmd that word is probably a misspelling
// of, ready to append to a message.
func suggest(cmd *cobra.Command, word string) string {
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if found := cmd.SuggestionsFor(word); len(found) > 0 {
		return "; did you mean " + strings.Join(found, " or ") + "?"
	}
	return ""
}

// usageArgs makes every argument check in the tree report a usage error.
// cobra's own checks, such as cobra.ExactArgs, return plain errors, which
// would otherwise exit 1, the code for a target that failed.
func usageArgs(cmd *cobra.Command) {
	if check := cmd.Args; check != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			return exitcode.Default(exitcode.Usage, check(c, args))
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
		return report(ctx, streams, err)
	}
	return exitcode.OK
}

// report prints an error and returns the exit code it asks for. A dry run
// that stopped on purpose is not an error.
//
// An error often quotes what a node, a BMC, a group source or an agent
// said, so its message is escaped with output.EscapeText like any other
// untrusted text. The lines clusterctl breaks a message into, such as the
// problems of a configuration file, are kept.
//
// A command that failed once ctx, the context a signal cancels, had ended
// was interrupted, whatever its error says: a request the interrupt stopped
// fails as it happens to, and some paths keep only an error's text, so the
// cancellation cannot always be found in the chain.
func report(ctx context.Context, streams app.Streams, err error) int {
	if safety.IsDryRun(err) {
		return exitcode.OK
	}
	if errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintln(streams.Err, "clusterctl: interrupted")
		return exitcode.Interrupted
	}
	if ctx.Err() != nil {
		_, _ = fmt.Fprintf(streams.Err, "clusterctl: interrupted: %s\n", output.EscapeText(err.Error()))
		return exitcode.Interrupted
	}
	// The exit code is what a caller acts on; a message that cannot be
	// written changes nothing about it.
	_, _ = fmt.Fprintf(streams.Err, "clusterctl: %s\n", output.EscapeText(err.Error()))
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
