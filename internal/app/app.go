// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package app wires the configuration, the transport and the node model into
// the context every command runs against.
//
// A command receives an App and asks it for targets, node sets and an
// executor. Nothing in a command reads the configuration or builds an ssh
// command line itself, so behaviour such as the dry run, the safety gate and
// the output format works the same everywhere.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/credentials"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/groups"
	"github.com/GSI-HPC/clusterctl/internal/hostname"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/naming"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Streams are the input and output a command talks to. Tests replace them
// with buffers.
type Streams struct {
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	IsTTY    bool
	StateDir string
	CacheDir string
}

// DefaultStreams returns the process streams.
//
// Whether there is a terminal decides whether a destructive command may ask
// for a confirmation or has to refuse, so it is asked of the terminal itself
// rather than inferred from the file mode: /dev/null is a character device
// too, and a command run with stdin closed would otherwise be prompted and
// then read an immediate end of file.
//
// A state or cache directory that cannot be known is left empty, and New
// refuses to run without it.
func DefaultStreams() Streams {
	stateDir, _ := config.StateDir()
	cacheDir, _ := config.CacheDir()
	return Streams{
		In:       os.Stdin,
		Out:      os.Stdout,
		Err:      os.Stderr,
		IsTTY:    term.IsTerminal(int(os.Stdin.Fd())),
		StateDir: stateDir,
		CacheDir: cacheDir,
	}
}

// Options are the global flags, resolved before a command runs.
type Options struct {
	// ConfigFiles replaces the search path when it is set.
	ConfigFiles []string
	// Context selects the context.
	Context string
	// Nodes is the default node set, from -n or the environment.
	Nodes string
	// NodesFromFlag says Nodes came from -n, so a node set given as an
	// argument as well is a contradiction rather than an override.
	NodesFromFlag bool
	// Format is the -o value.
	Format string
	// Set are the --set assignments.
	Set map[string]string
	// DryRun reports what would be done and changes nothing.
	DryRun bool
	// AssumeYes answers the confirmations in advance.
	AssumeYes bool
	// Force allows protected hosts to be touched.
	Force bool
	// Fanout overrides how many hosts are worked on at once.
	Fanout int
	// Env reads environment variables; nil reads the process environment.
	Env func(string) string
	// Runner replaces the ssh transport for everything a command runs. A
	// dry run sets it to a recorder, and the tests use it to drive the
	// commands without a cluster.
	Runner transport.Runner
}

// App is the resolved context a command runs against.
type App struct {
	Streams

	// Resolved is the merged configuration with its provenance.
	Resolved *config.Resolved
	// Spec is the merged configuration.
	Spec v1alpha1.EffectiveSpec
	// Namer turns node names into host names.
	Namer *naming.Namer
	// Inventory holds what is known about the nodes.
	Inventory *inventory.Inventory
	// Groups resolves @group references.
	Groups *groups.Resolver
	// SSH is the transport client; Runner is what commands run through,
	// which is a recorder during a dry run.
	SSH    *transport.Client
	Runner transport.Runner
	// ReadRunner runs the read-only lookups a command makes before it
	// changes anything, such as asking Slurm whether a node runs a job. It
	// is the real transport even in a dry run, so that the dry run reaches
	// the same decision the real run would.
	ReadRunner transport.Runner
	// DryRunRecorder holds what a dry run would have sent.
	DryRunRecorder *transport.Recorder
	// Gate guards the destructive commands.
	Gate *safety.Gate
	// Format is the output format.
	Format output.Format

	opts            Options
	ctx             context.Context
	credentials     *credentials.Resolver
	credentialsOnce sync.Once
	secrets         secretStore

	machinesOnce sync.Once
	machines     *machines
}

// ensureDirs creates the state and cache directories, or checks the ones
// that are there: the ssh configuration, the certificate pins and the group
// listings kept in them are trusted, so nobody else may be able to write
// them.
func ensureDirs(streams Streams) error {
	for _, dir := range []struct {
		name, path string
		find       func() (string, error)
	}{
		{"state", streams.StateDir, config.StateDir},
		{"cache", streams.CacheDir, config.CacheDir},
	} {
		if dir.path == "" {
			_, err := dir.find()
			if err == nil {
				err = errors.New("none was given")
			}
			return fmt.Errorf("there is no %s directory: %w", dir.name, err)
		}
		if err := fileutil.EnsureDir(dir.path); err != nil {
			return fmt.Errorf("the %s directory cannot be used: %w", dir.name, err)
		}
	}
	return nil
}

// New resolves the configuration and builds the command context.
func New(ctx context.Context, streams Streams, opts Options) (*App, error) {
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}

	var (
		files []string
		err   error
	)
	if len(opts.ConfigFiles) > 0 {
		// --config accepts files and directories, the same way the search
		// path does.
		files, err = config.ExpandEntries(opts.ConfigFiles)
	} else {
		files, err = config.SearchPath(env)
	}
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if len(files) == 0 {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no configuration was found; run \"clusterctl config init\" to write a first one, "+
				"put a Config and a Site document in %s, or point %s at them",
			strings.Join(config.ConfigDirs(), " or "), config.EnvConfig)
	}

	bundle, err := config.Load(files)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}

	resolved, err := bundle.Resolve(config.ResolveOptions{
		Context: opts.Context,
		Env:     env,
		Set:     opts.Set,
		Fanout:  opts.Fanout,
	})
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}

	format, err := output.ParseFormat(opts.Format)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if err := ensureDirs(streams); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}

	a := &App{
		Streams:  streams,
		ctx:      ctx,
		Resolved: resolved,
		Spec:     resolved.Spec,
		Format:   format,
		opts:     opts,
	}
	// --fanout is applied as the flags layer of the configuration, so that
	// config explain reports it; the built-in default is in defaults.yaml.
	// This only catches a fanout.max of zero written somewhere.
	if a.Spec.Fanout.Max == 0 {
		a.Spec.Fanout.Max = fanout.DefaultMax
	}
	if err := a.absoluteCommandLinePaths(); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}

	if a.Namer, err = naming.New(a.Spec.Naming, a.Spec.Domains); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if a.Inventory, err = a.loadInventory(bundle); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}

	// The generated ssh configuration outlives this process and is read
	// again by every ssh started from anywhere, so every path in it is
	// absolute. Includes are resolved against the Site directory like every
	// other configured path; a pattern is left for ssh to expand.
	sshSpec := a.Spec.SSH
	sshSpec.Include = make([]string, 0, len(a.Spec.SSH.Include))
	for _, include := range a.Spec.SSH.Include {
		sshSpec.Include = append(sshSpec.Include, absolute(a.Path(include)))
	}
	sshOpts := transport.Options{
		SSH:            sshSpec,
		Roles:          a.Spec.Hosts,
		StateDir:       absolute(streams.StateDir),
		KnownHostsFile: absolute(a.Path(a.Spec.SSH.KnownHostsFile)),
		DefaultUser:    a.Spec.DefaultUser,
	}
	if err := sshOpts.Validate(); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	a.SSH = transport.New(sshOpts)
	a.Runner = a.SSH
	if opts.Runner != nil {
		a.Runner = opts.Runner
	}
	if opts.DryRun {
		a.DryRunRecorder = &transport.Recorder{}
		a.Runner = a.DryRunRecorder
	}

	a.ReadRunner = transport.Runner(a.SSH)
	if opts.Runner != nil {
		a.ReadRunner = opts.Runner
	}
	a.Groups = groups.New(groups.Options{
		Spec:      a.Spec.Groups,
		Inventory: a.Inventory,
		Runner:    a.ReadRunner, // group lookups only read, so a dry run still resolves them
		Target:    a.Role,
		CacheDir:  streams.CacheDir,
		Scope:     a.cacheScope(),
		Timeout:   a.Timeout().Get(),
		Context:   ctx,
	})

	if a.Gate, err = safety.NewGate(a.Spec.Safety, a.protectedHosts); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if a.Inventory.Len() > 0 {
		a.Gate.Known = a.Inventory.NodeSet()
	}
	// Entries without a group need nothing but the configuration, so a
	// wrong one is reported by every command, config validate included.
	// Entries with one are resolved when a command is about to change
	// something, so that a group source that cannot be asked does not stop
	// the commands that only look.
	if !slices.ContainsFunc(a.Spec.Safety.ProtectedHosts, func(expr string) bool {
		return strings.Contains(expr, "@")
	}) {
		if _, err := a.Gate.Protected(); err != nil {
			return nil, err
		}
	}
	a.Gate.AssumeYes = opts.AssumeYes
	a.Gate.Force = opts.Force
	a.Gate.DryRun = opts.DryRun
	a.Gate.Interactive = streams.IsTTY
	a.Gate.In = streams.In
	a.Gate.Out = streams.Err

	return a, nil
}

// absoluteCommandLinePaths makes the relative paths that were given in the
// environment or with --set absolute against the working directory. Path
// resolves what is left against the directory of the Site document, which is
// right for a path written in a document: CLUSTERCTL_KNOWN_HOSTS=known_hosts
// means the file in the directory the command was run from, not one in the
// site checkout.
func (a *App) absoluteCommandLinePaths() error {
	var wd string
	abs := func(configPath string, value *string) error {
		if *value == "" || !a.Resolved.FromCommandLine(configPath) {
			return nil
		}
		expanded := config.ExpandPath(*value, "")
		if filepath.IsAbs(expanded) {
			*value = expanded
			return nil
		}
		if wd == "" {
			var err error
			if wd, err = os.Getwd(); err != nil {
				return fmt.Errorf("%s is relative and the working directory is not known: %w", configPath, err)
			}
		}
		*value = filepath.Join(wd, expanded)
		return nil
	}
	if err := abs("ssh.knownHostsFile", &a.Spec.SSH.KnownHostsFile); err != nil {
		return err
	}
	if err := abs("bmc.redfish.pinStore", &a.Spec.BMC.Redfish.PinStore); err != nil {
		return err
	}
	for i := range a.Spec.Workstation.Identities {
		if err := abs("workstation.identities", &a.Spec.Workstation.Identities[i]); err != nil {
			return err
		}
	}
	for i := range a.Spec.Services.Cinc.Secrets {
		if err := abs("services.cinc.secrets", &a.Spec.Services.Cinc.Secrets[i].Source); err != nil {
			return err
		}
	}
	return nil
}

// loadInventory builds the node inventory from the documents the cluster
// names, or from every one that was loaded when it names none.
func (a *App) loadInventory(bundle *config.Bundle) (*inventory.Inventory, error) {
	wanted := a.Resolved.Bundle.Clusters[a.Resolved.ClusterName]
	var names []string
	if wanted != nil {
		var spec struct {
			Spec struct {
				Inventories []string `json:"inventories"`
			} `json:"spec"`
		}
		_ = decode(wanted.Data, &spec)
		names = spec.Spec.Inventories
	}
	if len(names) == 0 {
		for name := range bundle.Inventories {
			names = append(names, name)
		}
		sort.Strings(names)
	}

	specs := make([]v1alpha1.NodeInventorySpec, 0, len(names))
	for _, name := range names {
		doc, ok := bundle.Inventories[name]
		if !ok {
			return nil, fmt.Errorf("cluster %q names inventory %q, which no document defines",
				a.Resolved.ClusterName, name)
		}
		var inv v1alpha1.NodeInventory
		if err := decode(doc.Data, &inv); err != nil {
			return nil, fmt.Errorf("%s: %w", doc.File, err)
		}
		specs = append(specs, inv.Spec)
	}
	return inventory.New(specs...)
}

// Path resolves a configured path against the directory of the site
// document, so that a site can be kept in version control and moved.
func (a *App) Path(path string) string {
	if path == "" {
		return ""
	}
	return config.ExpandPath(path, a.Resolved.BaseDir)
}

// absolute makes a path absolute against the working directory, which is
// what a relative --config or CLUSTERCTL_CONFIG was given relative to.
func absolute(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Role returns the target of an infrastructure role.
func (a *App) Role(name string) (transport.Target, error) {
	if name == "" {
		return transport.Target{}, exitcode.Errorf(exitcode.Usage, "no host role was given")
	}
	role, ok := a.Spec.Hosts[name]
	if !ok {
		return transport.Target{}, exitcode.Errorf(exitcode.Usage,
			"unknown host role %q; the site defines %s", name, strings.Join(a.RoleNames(), ", "))
	}
	if role.Host == "" {
		return transport.Target{}, exitcode.Errorf(exitcode.Usage, "host role %q has no host name", name)
	}
	return transport.Target{
		Name:         name,
		Host:         role.Host,
		User:         role.User,
		Role:         name,
		ForwardAgent: role.ForwardAgent,
		ForwardX11:   role.ForwardX11,
	}, nil
}

// RoleNames lists the configured host roles in sorted order.
func (a *App) RoleNames() []string {
	out := make([]string, 0, len(a.Spec.Hosts))
	for name := range a.Spec.Hosts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Node returns the target of a compute node, whose host name comes from the
// naming rules.
func (a *App) Node(name string) (transport.Target, error) {
	host, err := a.Namer.FQDN(name)
	if err != nil {
		return transport.Target{}, exitcode.Wrap(exitcode.Usage, err)
	}
	return transport.Target{Name: name, Host: host, User: a.Spec.DefaultUser}, nil
}

// NodeTargets returns a target for each node of a set, in expansion order.
func (a *App) NodeTargets(ns *nodeset.NodeSet) ([]transport.Target, error) {
	out := make([]transport.Target, 0, ns.Len())
	for _, name := range ns.Expand() {
		target, err := a.Node(name)
		if err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, nil
}

// Select parses a node set expression, resolving group references.
//
// An empty expression falls back to what -n or the environment named, so a
// session can select a set once and then work with it. An expression replaces
// a set from the environment, which is only a default, but not one from -n:
// the two together are refused rather than one of them dropped.
func (a *App) Select(expr string) (*nodeset.NodeSet, error) {
	if strings.TrimSpace(expr) == "" {
		expr = a.opts.Nodes
	} else if a.opts.NodesFromFlag {
		return nil, exitcode.Errorf(exitcode.Usage,
			"nodes were given both as an argument (%s) and with -n (%s); give them once", expr, a.opts.Nodes)
	}
	if strings.TrimSpace(expr) == "" {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no nodes were selected; pass -n or set %s", config.EnvNodes)
	}
	ns, err := nodeset.ParseWith(expr, a.Groups)
	if err != nil {
		// A group source that could not be asked has already said so with
		// an exit code of its own; only what carries none is the
		// expression's fault.
		var coded *exitcode.Error
		if errors.As(err, &coded) {
			return nil, err
		}
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if ns.IsEmpty() {
		return nil, exitcode.Errorf(exitcode.Usage, "%q names no node", expr)
	}
	if ns, err = a.canonicalize(ns); err != nil {
		return nil, err
	}
	if err := checkHostNames(ns); err != nil {
		return nil, err
	}
	return ns, nil
}

// checkHostNames refuses a selection with a name that is not a host name.
//
// A node name ends up as an ssh destination and inside a Redfish URL, where a
// leading "-" is an option and ":", "@", "/", "?" and "#" redirect the
// request. It may have been typed, come from a group source or an inventory
// file, or have been chosen by an agent, so it is checked here, once, for
// every command.
func checkHostNames(ns *nodeset.NodeSet) error {
	for _, name := range ns.Expand() {
		if err := hostname.Check(name); err != nil {
			return exitcode.Wrap(exitcode.Usage, fmt.Errorf("node %w", err))
		}
	}
	return nil
}

// canonicalize replaces each name with the one the inventory uses for that
// machine, so that one machine is one target however it was written.
//
// Padding is a display property, so exe1 and exe0001 name the same machine.
// Case and a final dot do not change a host name, and the host name, the
// service processor name and the addresses of a node all reach it. An
// administrator who types any of them should reach the host the site wrote
// down, see it under the name the site gave it, and be stopped by the gate
// if it is protected. Names that turn out to be one machine become one
// target, so that it is not reset twice at once.
func (a *App) canonicalize(ns *nodeset.NodeSet) (*nodeset.NodeSet, error) {
	out := nodeset.New()
	for _, name := range ns.Expand() {
		machine := a.machine(name)
		if err := out.Add(machine); err != nil {
			return nil, exitcode.Errorf(exitcode.Usage, "node %q, which is %q, cannot be selected: %w",
				name, machine, err)
		}
	}
	return out, nil
}

// SelectOptional is Select without the requirement that anything is
// selected; it returns nil when nothing was named.
func (a *App) SelectOptional(expr string) (*nodeset.NodeSet, error) {
	if strings.TrimSpace(expr) == "" && strings.TrimSpace(a.opts.Nodes) == "" {
		return nil, nil
	}
	return a.Select(expr)
}

// Executor returns a fan-out executor bound to the configured limits.
func (a *App) Executor() *fanout.Executor {
	return &fanout.Executor{Runner: a.Runner, Max: a.Spec.Fanout.Max}
}

// Timeout returns the command timeout for remote execution.
func (a *App) Timeout() v1alpha1.Duration { return a.Spec.Fanout.CommandTimeout }

// DryRun reports whether nothing may actually be changed.
func (a *App) DryRun() bool { return a.opts.DryRun }

// Print renders a result in the selected format. A jq program stops with the
// command's context, when the command is interrupted or cancelled.
func (a *App) Print(r output.Result) error {
	return a.Format.WriteContext(a.Context(), a.Out, r)
}

// Printf writes a message to the error stream, which is where progress and
// notes go so that they never mix into parsed output.
func (a *App) Printf(format string, args ...any) {
	// Progress goes to the error stream. A note that cannot be written is
	// not worth failing a command that is otherwise succeeding.
	_, _ = fmt.Fprintf(a.Err, format, args...)
}

// StatePath returns a path inside the state directory.
func (a *App) StatePath(parts ...string) string {
	return filepath.Join(append([]string{a.StateDir}, parts...)...)
}

// decode converts a parsed document into a typed value.
func decode(data map[string]any, target any) error {
	return config.Decode(data, target)
}

// Context returns the context bounding the command's remote work.
func (a *App) Context() context.Context {
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}

// IdentityPaths returns the configured age identities as absolute paths.
func (a *App) IdentityPaths() []string {
	out := make([]string, 0, len(a.Spec.Workstation.Identities))
	for _, p := range a.Spec.Workstation.Identities {
		out = append(out, a.Path(p))
	}
	return out
}
