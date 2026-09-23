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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/credentials"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/groups"
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
func DefaultStreams() Streams {
	return Streams{
		In:       os.Stdin,
		Out:      os.Stdout,
		Err:      os.Stderr,
		IsTTY:    term.IsTerminal(int(os.Stdin.Fd())),
		StateDir: config.StateDir(),
		CacheDir: config.CacheDir(),
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
	// DryRunRecorder holds what a dry run would have sent.
	DryRunRecorder *transport.Recorder
	// Gate guards the destructive commands.
	Gate *safety.Gate
	// Format is the output format.
	Format output.Format

	opts        Options
	ctx         context.Context
	credentials *credentials.Resolver
	secrets     secretStore
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
			"no configuration was found; put a Config and a Site document in %s or point %s at them",
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
	})
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}

	format, err := output.ParseFormat(opts.Format)
	if err != nil {
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
	if a.Spec.Fanout.Max == 0 {
		a.Spec.Fanout.Max = fanout.DefaultMax
	}
	if opts.Fanout > 0 {
		a.Spec.Fanout.Max = opts.Fanout
	}

	if a.Namer, err = naming.New(a.Spec.Naming, a.Spec.Domains); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if a.Inventory, err = a.loadInventory(bundle); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}

	a.SSH = transport.New(transport.Options{
		SSH:            a.Spec.SSH,
		Roles:          a.Spec.Hosts,
		StateDir:       streams.StateDir,
		KnownHostsFile: a.Path(a.Spec.SSH.KnownHostsFile),
		DefaultUser:    a.Spec.DefaultUser,
	})
	a.Runner = a.SSH
	if opts.Runner != nil {
		a.Runner = opts.Runner
	}
	if opts.DryRun {
		a.DryRunRecorder = &transport.Recorder{}
		a.Runner = a.DryRunRecorder
	}

	groupRunner := transport.Runner(a.SSH)
	if opts.Runner != nil {
		groupRunner = opts.Runner
	}
	a.Groups = groups.New(groups.Options{
		Spec:      a.Spec.Groups,
		Inventory: a.Inventory,
		Runner:    groupRunner, // group lookups only read, so a dry run still resolves them
		Target:    a.Role,
		CacheDir:  streams.CacheDir,
		Context:   ctx,
	})

	if a.Gate, err = safety.NewGate(a.Spec.Safety); err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	a.Gate.AssumeYes = opts.AssumeYes
	a.Gate.Force = opts.Force
	a.Gate.DryRun = opts.DryRun
	a.Gate.Interactive = streams.IsTTY
	a.Gate.In = streams.In
	a.Gate.Out = streams.Err

	return a, nil
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
// session can select a set once and then work with it.
func (a *App) Select(expr string) (*nodeset.NodeSet, error) {
	if strings.TrimSpace(expr) == "" {
		expr = a.opts.Nodes
	}
	if strings.TrimSpace(expr) == "" {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no nodes were selected; pass -n or set %s", config.EnvNodes)
	}
	ns, err := nodeset.ParseWith(expr, a.Groups)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if ns.IsEmpty() {
		return nil, exitcode.Errorf(exitcode.Usage, "%q names no node", expr)
	}
	return a.canonicalize(ns), nil
}

// canonicalize replaces each name with the one the inventory uses for that
// host.
//
// Padding is a display property, so exe1 and exe0001 name the same machine.
// An administrator who types the short form should reach the host the site
// wrote down, and see it under the name the site gave it. A node the
// inventory does not know is left exactly as it was typed.
func (a *App) canonicalize(ns *nodeset.NodeSet) *nodeset.NodeSet {
	if a.Inventory == nil || a.Inventory.Len() == 0 {
		return ns
	}
	out := nodeset.New()
	for _, name := range ns.Expand() {
		if canonical, ok := a.Inventory.Resolve(name); ok {
			name = canonical
		}
		if err := out.Add(name); err != nil {
			return ns
		}
	}
	return out
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

// Print renders a result in the selected format.
func (a *App) Print(r output.Result) error {
	return a.Format.Write(a.Out, r)
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
