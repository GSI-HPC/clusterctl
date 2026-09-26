// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package groups resolves the @group references of a node set expression.
//
// A group source is one of three things: a table written in the
// configuration, one group per value of a node attribute, which is what the
// genders file used to provide, or the output of a command run on an
// infrastructure host, which is how the workload manager is asked what it
// currently knows.
package groups

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// Placeholders an exec source's argument vector may carry. They are
// substituted as whole arguments, never expanded by a shell, so a group name
// cannot turn into a command.
const (
	PlaceholderGroup = "$GROUP"
	PlaceholderNode  = "$NODE"
)

// TargetFunc resolves the host role an exec source runs on.
type TargetFunc func(role string) (transport.Target, error)

// ErrNotDefined is what a source answers when it definitely has no group of
// the name asked for. It is the only answer that lets a bare @group move on
// to the next source.
var ErrNotDefined = errors.New("group not defined")

// ErrCannotList is what a source answers when it has no way to list its
// groups, which is a property of its configuration rather than a failure.
var ErrCannotList = errors.New("the source cannot list its groups")

// sentinelError carries a message of its own and matches one sentinel.
type sentinelError struct {
	sentinel error
	msg      string
}

func (e *sentinelError) Error() string        { return e.msg }
func (e *sentinelError) Is(target error) bool { return target == e.sentinel }

func notDefined(format string, args ...any) error {
	return &sentinelError{sentinel: ErrNotDefined, msg: fmt.Sprintf(format, args...)}
}

// Resolver implements nodeset.Resolver over the configured group sources.
type Resolver struct {
	sources   map[string]v1alpha1.GroupSource
	def       string
	inventory *inventory.Inventory
	runner    transport.Runner
	target    TargetFunc
	cacheDir  string
	scope     string
	timeout   time.Duration
	ctx       context.Context

	mu sync.Mutex
	// cache holds the answers this resolver was given, and missing the
	// groups a source said it does not have. flights holds the lookups
	// under way, for a caller that asks meanwhile to wait for.
	cache   map[string]string
	missing map[string]error
	flights map[string]*flight
}

// flight is one lookup under way.
type flight struct {
	done chan struct{}
	expr string
	err  error
	// waiting counts the callers that wait for it, under the resolver's
	// lock.
	waiting int
}

// errAbandoned is what the callers waiting for a lookup are told when the
// caller making it did not finish, which only a panic does.
var errAbandoned = errors.New("the group lookup did not finish")

// Options configure a resolver.
type Options struct {
	// Spec is the group configuration of the cluster.
	Spec v1alpha1.GroupsSpec
	// Inventory backs the attribute sources.
	Inventory *inventory.Inventory
	// Runner runs the commands of an exec source.
	Runner transport.Runner
	// Target resolves the host role an exec source runs on.
	Target TargetFunc
	// CacheDir holds the resolved groups of sources that ask to be cached.
	// An empty directory keeps the cache in memory only.
	CacheDir string
	// Scope names what the resolver answers for, such as the site, cluster
	// and context. The cache directory is shared by every configuration a
	// user has, so an entry is only reused by a resolver of the same scope,
	// for the same target host and the same command.
	Scope string
	// Timeout bounds each command an exec source runs. It is enforced on the
	// target, the way every remote command is.
	Timeout time.Duration
	// Context bounds the commands an exec source runs. The nodeset resolver
	// interface carries no context of its own, so it is held here.
	Context context.Context
}

// New builds a resolver.
func New(opts Options) *Resolver {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	def := opts.Spec.DefaultSource
	if def == "" && len(opts.Spec.Sources) == 1 {
		for name := range opts.Spec.Sources {
			def = name
		}
	}
	return &Resolver{
		sources:   opts.Spec.Sources,
		def:       def,
		inventory: opts.Inventory,
		runner:    opts.Runner,
		target:    opts.Target,
		cacheDir:  opts.CacheDir,
		scope:     opts.Scope,
		timeout:   opts.Timeout,
		ctx:       ctx,
		cache:     map[string]string{},
		missing:   map[string]error{},
		flights:   map[string]*flight{},
	}
}

// DefaultSource implements nodeset.Resolver.
func (r *Resolver) DefaultSource() string { return r.def }

// Sources lists the configured source names in sorted order.
func (r *Resolver) Sources() []string {
	return slices.Sorted(maps.Keys(r.sources))
}

// Resolve implements nodeset.Resolver.
//
// A bare @group reference looks in the default source first and then in
// every other source, so the groups of a small installation need no prefix.
// The search moves on only when a source answers that it has no such group.
// A source that cannot be asked stops it: the group may well be that
// source's, and another source's group of the same name is not an answer.
func (r *Resolver) Resolve(source, group string) (string, error) {
	if source != "" {
		return r.resolveIn(r.ctx, source, group)
	}
	order := r.Sources()
	if r.def != "" {
		order = slices.DeleteFunc(order, func(name string) bool { return name == r.def })
		order = append([]string{r.def}, order...)
	}
	var tried []string
	for _, name := range order {
		expr, err := r.resolveIn(r.ctx, name, group)
		if err == nil {
			return expr, nil
		}
		if !errors.Is(err, ErrNotDefined) {
			if len(tried) > 0 {
				return "", fmt.Errorf("%w; the search for @%s stops at a source that cannot answer "+
					"(before it: %s)", err, group, strings.Join(tried, "; "))
			}
			return "", fmt.Errorf("%w; the search for @%s stops at a source that cannot answer", err, group)
		}
		tried = append(tried, err.Error())
	}
	return "", notDefined("no group source defines %q (sources: %s)", group, strings.Join(r.Sources(), ", "))
}

// resolveIn asks one source for one group, a lookup made under ctx.
func (r *Resolver) resolveIn(ctx context.Context, name, group string) (string, error) {
	src, ok := r.sources[name]
	if !ok {
		return "", fmt.Errorf("unknown group source %q (known: %s)", name, strings.Join(r.Sources(), ", "))
	}
	return r.lookup(ctx, cacheKey(name, "map", group), "resolve @"+name+":"+group, func(q *asking) (string, error) {
		return r.find(q, name, src, group)
	})
}

// find asks one source for one group.
func (r *Resolver) find(q *asking, name string, src v1alpha1.GroupSource, group string) (string, error) {
	var (
		expr string
		err  error
	)
	switch {
	case src.Static != nil:
		value, ok := src.Static[group]
		if !ok {
			return "", notDefined("source %q has no group %q", name, group)
		}
		expr = value
	case src.Attribute != "":
		if r.inventory == nil {
			return "", fmt.Errorf("source %q reads node attributes, but no inventory is loaded", name)
		}
		ns := r.inventory.WithAttribute(src.Attribute, group)
		if ns.IsEmpty() {
			return "", notDefined("source %q: no node has %s=%s", name, src.Attribute, group)
		}
		expr = ns.String()
	case src.Exec != nil:
		expr, err = r.execCached(q, name, src, src.Exec.Map, map[string]string{PlaceholderGroup: group})
		if err == nil && strings.TrimSpace(expr) == "" {
			err = fmt.Errorf("returned no nodes for group %q", group)
		}
		if err != nil {
			return "", r.execFailed(q, name, group, err)
		}
	default:
		return "", fmt.Errorf("group source %q defines nothing", name)
	}
	return expr, nil
}

// All implements nodeset.Resolver.
func (r *Resolver) All(source string) (string, error) {
	source = cmp.Or(source, r.def)
	src, ok := r.sources[source]
	if !ok {
		return "", fmt.Errorf("unknown group source %q (known: %s)", source, strings.Join(r.Sources(), ", "))
	}
	return r.lookup(r.ctx, cacheKey(source, "all", ""), "resolve @"+source+":*", func(q *asking) (string, error) {
		if src.Exec != nil && len(src.Exec.All) > 0 {
			out, err := r.execCached(q, source, src, src.Exec.All, nil)
			if err != nil {
				return "", fmt.Errorf("source %q: %w", source, err)
			}
			return out, nil
		}
		// Without a command of its own, the union of every group is what
		// the source knows.
		names, err := r.list(q.ctx, source)
		if err != nil {
			return "", err
		}
		parts := make([]string, 0, len(names))
		for _, group := range names {
			one, err := r.resolveIn(q.ctx, source, group)
			if err != nil {
				return "", err
			}
			parts = append(parts, one)
		}
		return strings.Join(parts, ","), nil
	})
}

// List implements nodeset.Resolver.
func (r *Resolver) List(source string) ([]string, error) {
	return r.list(r.ctx, source)
}

// list lists the groups of a source, a lookup made under ctx.
func (r *Resolver) list(ctx context.Context, source string) ([]string, error) {
	source = cmp.Or(source, r.def)
	src, ok := r.sources[source]
	if !ok {
		return nil, fmt.Errorf("unknown group source %q (known: %s)", source, strings.Join(r.Sources(), ", "))
	}

	switch {
	case src.Static != nil:
		// Not slices.Sorted, which returns nil for no groups: node groups
		// prints a source that has none as [], not null.
		out := make([]string, 0, len(src.Static))
		for name := range src.Static {
			out = append(out, name)
		}
		sort.Strings(out)
		return out, nil
	case src.Attribute != "":
		if r.inventory == nil {
			return nil, fmt.Errorf("source %q reads node attributes, but no inventory is loaded", source)
		}
		return r.inventory.AttributeValues(src.Attribute), nil
	case src.Exec != nil && len(src.Exec.List) > 0:
		out, err := r.lookup(ctx, cacheKey(source, "list", ""), "list the groups of @"+source, func(q *asking) (string, error) {
			out, err := r.execCached(q, source, src, src.Exec.List, nil)
			if err != nil {
				return "", fmt.Errorf("source %q: %w", source, err)
			}
			return out, nil
		})
		if err != nil {
			return nil, err
		}
		return fields(out), nil
	default:
		return nil, &sentinelError{sentinel: ErrCannotList,
			msg: fmt.Sprintf("group source %q cannot list its groups", source)}
	}
}

// execFailed decides what a failed or empty lookup in an exec source means.
//
// A command gives no reliable "no such group" of its own: sinfo prints
// nothing for a partition it does not have, and a host that does not answer
// prints nothing either. Only a source that can list its groups can say
// that a group is not one of them, and only when that listing succeeds.
func (r *Resolver) execFailed(q *asking, name, group string, err error) error {
	err = fmt.Errorf("source %q: %w", name, err)
	if exitcode.From(err) == exitcode.Transport || len(r.sources[name].Exec.List) == 0 {
		return err
	}
	// A cached listing may predate the group, and passing the search on to
	// another source's group of the same name is worse than asking again.
	// So the listing is asked for afresh, once for the resolver, however
	// many groups it finds missing: one made during this command is as new
	// as the answer that just failed, and only one that succeeded is kept.
	src := r.sources[name]
	out, listErr := r.lookup(q.ctx, cacheKey(name, "fresh list", ""), "list the groups of @"+name+" again",
		func(q *asking) (string, error) {
			return r.exec(q, src.Exec, src.Exec.List, nil)
		})
	if listErr != nil || slices.Contains(fields(out), group) {
		return err
	}
	return notDefined("source %q has no group %q", name, group)
}

// GroupsOf returns the groups of one node, across every source that can
// answer the question.
//
// A source that fails does not hide what the others found: the memberships
// that were found are returned together with an error naming each source
// that could not be asked. A source that has no way to list its groups is
// left out without an error.
func (r *Resolver) GroupsOf(node string) (map[string][]string, error) {
	return r.GroupsOfContext(r.ctx, node)
}

// GroupsOfContext is GroupsOf with its lookups made, and reported, under
// ctx, such as the target of the node they are for in a pool, rather than
// under the resolver's own context. A lookup another caller made first is
// reported where that one made it.
func (r *Resolver) GroupsOfContext(ctx context.Context, node string) (map[string][]string, error) {
	out := map[string][]string{}
	var errs []error
	for _, name := range r.Sources() {
		member, err := r.groupsIn(ctx, name, node)
		if len(member) > 0 {
			out[name] = member
		}
		if err != nil && !errors.Is(err, ErrCannotList) {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// groupsIn returns the groups of one node in one source, looked up under
// ctx.
func (r *Resolver) groupsIn(ctx context.Context, name, node string) ([]string, error) {
	src := r.sources[name]
	if src.Exec != nil && len(src.Exec.Reverse) > 0 {
		answer, err := r.exec(&asking{ctx: ctx}, src.Exec, src.Exec.Reverse, map[string]string{PlaceholderNode: node})
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", name, err)
		}
		return fields(answer), nil
	}

	groups, err := r.list(ctx, name)
	if err != nil {
		return nil, err
	}
	var (
		member []string
		errs   []error
	)
	for _, group := range groups {
		expr, err := r.resolveIn(ctx, name, group)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ns, err := nodeset.ParseWith(expr, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("source %q, group %q: %w", name, group, err))
			continue
		}
		if ns.Contains(node) {
			member = append(member, group)
		}
	}
	return member, errors.Join(errs...)
}

// execCached is exec for a lookup the source allows to be cached.
func (r *Resolver) execCached(q *asking, name string, src v1alpha1.GroupSource, argv []string, vars map[string]string) (string, error) {
	target, command, err := r.prepare(src.Exec, argv, vars)
	if err != nil {
		return "", err
	}
	ttl := src.CacheTTL.Get()
	if ttl <= 0 || r.cacheDir == "" {
		return r.run(q, target, command)
	}
	key, dir := r.diskKey(name, target, command), filepath.Join(r.cacheDir, "groups")
	if expr, ok := fileutil.ReadCache(dir, key, ttl); ok {
		q.cache = "disk"
		return string(expr), nil
	}
	expr, err := r.run(q, target, command)
	if err != nil {
		return "", err
	}
	fileutil.WriteCache(dir, key, []byte(expr))
	return expr, nil
}

// exec runs one command of an exec source, substituting the placeholders as
// whole arguments.
func (r *Resolver) exec(q *asking, src *v1alpha1.ExecGroupSource, argv []string, vars map[string]string) (string, error) {
	target, command, err := r.prepare(src, argv, vars)
	if err != nil {
		return "", err
	}
	return r.run(q, target, command)
}

// prepare resolves the target of an exec source and substitutes the
// placeholders of one command.
func (r *Resolver) prepare(src *v1alpha1.ExecGroupSource, argv []string, vars map[string]string) (transport.Target, []string, error) {
	if len(argv) == 0 {
		return transport.Target{}, nil, fmt.Errorf("the source defines no command for this lookup")
	}
	if r.runner == nil {
		return transport.Target{}, nil, fmt.Errorf("group sources that run commands need a transport")
	}
	if src.Role == "" {
		return transport.Target{}, nil, exitcode.Errorf(exitcode.Usage,
			"the source names no host role to run its commands on; set exec.role")
	}
	target := transport.Target{Name: src.Role, Role: src.Role}
	if r.target != nil {
		resolved, err := r.target(src.Role)
		if err != nil {
			return transport.Target{}, nil, err
		}
		target = resolved
	}

	command := make([]string, len(argv))
	for i, arg := range argv {
		if value, ok := vars[arg]; ok {
			command[i] = value
			continue
		}
		command[i] = arg
	}
	return target, command, nil
}

// run runs one prepared command and returns its output as a node set list.
//
// An error already carrying an exit code, such as the transport failure of
// an ssh that could not connect, keeps it. A command that failed on a host
// that answered is that host's failure, reported with what it printed.
func (r *Resolver) run(q *asking, target transport.Target, command []string) (string, error) {
	q.cache = "miss"
	result, err := r.runner.Run(q.ctx, target, transport.Request{
		Argv:    command,
		Timeout: r.timeout,
		TTY:     transport.TTYNone,
	})
	if err != nil {
		return "", err
	}
	if err := result.Check(command[0]); err != nil {
		return "", err
	}
	return strings.Join(fields(result.Stdout), ","), nil
}

// fields splits command output on whitespace and commas, which is how the
// Slurm and genders style tools print a list.
func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ','
	})
}

// cacheKey identifies one lookup in memory. A resolver serves one scope, so
// the source and the group are enough.
func cacheKey(source, kind, group string) string {
	return source + "\x00" + kind + "\x00" + group
}

// asking is one lookup under way: the context the commands it sends run
// under, which carries its span, and where its answer came from, for the
// span: "disk" for the disk cache, "miss" for a command it sent, and nothing
// for an answer from the configuration or the inventory.
type asking struct {
	ctx   context.Context
	cache string
}

// lookup answers the lookup key names, running find for it at most once
// however many callers ask.
//
// A caller that asks while find runs waits for its answer rather than
// sending the command again, so the workers of a pool that all need one
// group cost one command between them. What find answered is kept for the
// resolver's life, which is one command, when it is a group's nodes or that
// the source has no such group. Any other failure is handed to the callers
// that waited for it and forgotten: a host that did not answer, or a
// command that was interrupted, says nothing about the source's groups, and
// the next caller asks again. find runs under the resolver's context, so it
// ends with the command, and so does every wait for it.
//
// The lookup that runs find is reported under ctx as a hidden call, name,
// and the commands it sends as calls under that; a caller answered from
// memory, or by the lookup of another, makes none. A source that has no
// such group has answered, so its lookup ends well: the search goes on.
func (r *Resolver) lookup(ctx context.Context, key, name string, find func(q *asking) (string, error)) (string, error) {
	r.mu.Lock()
	if expr, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return expr, nil
	}
	if err, ok := r.missing[key]; ok {
		r.mu.Unlock()
		return "", err
	}
	if f, ok := r.flights[key]; ok {
		f.waiting++
		r.mu.Unlock()
		<-f.done
		return f.expr, f.err
	}
	f := &flight{done: make(chan struct{}), err: errAbandoned}
	r.flights[key] = f
	r.mu.Unlock()

	ctx, span := progress.Start(ctx, progress.KindCall, name, progress.WithFlags(progress.Hidden))
	q := &asking{ctx: ctx}
	defer func() {
		r.mu.Lock()
		delete(r.flights, key)
		switch {
		case f.err == nil:
			r.cache[key] = f.expr
		case errors.Is(f.err, ErrNotDefined):
			r.missing[key] = f.err
		}
		r.mu.Unlock()
		close(f.done)
		if errors.Is(f.err, ErrNotDefined) {
			span.End(nil, progress.Cache(q.cache), progress.Message("no such group"))
		} else {
			span.End(f.err, progress.Cache(q.cache))
		}
	}()
	f.expr, f.err = find(q)
	return f.expr, f.err
}

// diskKey identifies one command of one source on disk: the scope the
// resolver serves, the host it runs on and the exact argument vector, which
// carries the group name. Nothing is replaced or shortened, so no two
// lookups share an entry unless they would send the same command to the
// same host for the same cluster.
func (r *Resolver) diskKey(source string, target transport.Target, command []string) any {
	return struct {
		Scope   string   `json:"scope"`
		Source  string   `json:"source"`
		Host    string   `json:"host"`
		User    string   `json:"user"`
		Role    string   `json:"role"`
		Command []string `json:"command"`
	}{r.scope, source, target.Host, target.User, target.Role, command}
}
