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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
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

// Resolver implements nodeset.Resolver over the configured group sources.
type Resolver struct {
	sources   map[string]v1alpha1.GroupSource
	def       string
	inventory *inventory.Inventory
	runner    transport.Runner
	target    TargetFunc
	cacheDir  string
	ctx       context.Context

	mu    sync.Mutex
	cache map[string]string
}

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
		ctx:       ctx,
		cache:     map[string]string{},
	}
}

// DefaultSource implements nodeset.Resolver.
func (r *Resolver) DefaultSource() string { return r.def }

// Sources lists the configured source names in sorted order.
func (r *Resolver) Sources() []string {
	out := make([]string, 0, len(r.sources))
	for name := range r.sources {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Resolve implements nodeset.Resolver.
//
// A bare @group reference looks in the default source first and then in
// every other source, so the groups of a small installation need no prefix.
func (r *Resolver) Resolve(source, group string) (string, error) {
	if source != "" {
		return r.resolveIn(source, group)
	}
	if r.def != "" {
		if expr, err := r.resolveIn(r.def, group); err == nil {
			return expr, nil
		}
	}
	for _, name := range r.Sources() {
		if name == r.def {
			continue
		}
		if expr, err := r.resolveIn(name, group); err == nil {
			return expr, nil
		}
	}
	return "", fmt.Errorf("no group source defines %q (sources: %s)", group, strings.Join(r.Sources(), ", "))
}

func (r *Resolver) resolveIn(name, group string) (string, error) {
	src, ok := r.sources[name]
	if !ok {
		return "", fmt.Errorf("unknown group source %q (known: %s)", name, strings.Join(r.Sources(), ", "))
	}
	if expr, ok := r.cached(name, "map", group, src); ok {
		return expr, nil
	}

	var (
		expr string
		err  error
	)
	switch {
	case src.Static != nil:
		value, ok := src.Static[group]
		if !ok {
			return "", fmt.Errorf("source %q has no group %q", name, group)
		}
		expr = value
	case src.Attribute != "":
		if r.inventory == nil {
			return "", fmt.Errorf("source %q reads node attributes, but no inventory is loaded", name)
		}
		ns := r.inventory.WithAttribute(src.Attribute, group)
		if ns.IsEmpty() {
			return "", fmt.Errorf("no node has %s=%s", src.Attribute, group)
		}
		expr = ns.String()
	case src.Exec != nil:
		expr, err = r.exec(src.Exec, src.Exec.Map, map[string]string{PlaceholderGroup: group})
		if err != nil {
			return "", fmt.Errorf("source %q: %w", name, err)
		}
		if strings.TrimSpace(expr) == "" {
			return "", fmt.Errorf("source %q returned no nodes for group %q", name, group)
		}
	default:
		return "", fmt.Errorf("group source %q defines nothing", name)
	}

	r.store(name, "map", group, src, expr)
	return expr, nil
}

// All implements nodeset.Resolver.
func (r *Resolver) All(source string) (string, error) {
	if source == "" {
		source = r.def
	}
	src, ok := r.sources[source]
	if !ok {
		return "", fmt.Errorf("unknown group source %q (known: %s)", source, strings.Join(r.Sources(), ", "))
	}
	if expr, ok := r.cached(source, "all", "", src); ok {
		return expr, nil
	}

	var expr string
	switch {
	case src.Exec != nil && len(src.Exec.All) > 0:
		out, err := r.exec(src.Exec, src.Exec.All, nil)
		if err != nil {
			return "", err
		}
		expr = out
	default:
		// Without a command of its own, the union of every group is what
		// the source knows.
		names, err := r.List(source)
		if err != nil {
			return "", err
		}
		parts := make([]string, 0, len(names))
		for _, group := range names {
			one, err := r.resolveIn(source, group)
			if err != nil {
				return "", err
			}
			parts = append(parts, one)
		}
		expr = strings.Join(parts, ",")
	}
	r.store(source, "all", "", src, expr)
	return expr, nil
}

// List implements nodeset.Resolver.
func (r *Resolver) List(source string) ([]string, error) {
	if source == "" {
		source = r.def
	}
	src, ok := r.sources[source]
	if !ok {
		return nil, fmt.Errorf("unknown group source %q (known: %s)", source, strings.Join(r.Sources(), ", "))
	}

	switch {
	case src.Static != nil:
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
		out, err := r.exec(src.Exec, src.Exec.List, nil)
		if err != nil {
			return nil, err
		}
		return fields(out), nil
	default:
		return nil, fmt.Errorf("group source %q cannot list its groups", source)
	}
}

// GroupsOf returns the groups of one node, across every source that can
// answer the question.
func (r *Resolver) GroupsOf(node string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, name := range r.Sources() {
		src := r.sources[name]
		switch {
		case src.Exec != nil && len(src.Exec.Reverse) > 0:
			answer, err := r.exec(src.Exec, src.Exec.Reverse, map[string]string{PlaceholderNode: node})
			if err != nil {
				return nil, err
			}
			if groups := fields(answer); len(groups) > 0 {
				out[name] = groups
			}
		default:
			groups, err := r.List(name)
			if err != nil {
				continue
			}
			var member []string
			for _, group := range groups {
				expr, err := r.resolveIn(name, group)
				if err != nil {
					continue
				}
				ns, err := nodeset.ParseWith(expr, r)
				if err != nil {
					continue
				}
				if ns.Contains(node) {
					member = append(member, group)
				}
			}
			if len(member) > 0 {
				out[name] = member
			}
		}
	}
	return out, nil
}

// exec runs one command of an exec source, substituting the placeholders as
// whole arguments.
func (r *Resolver) exec(src *v1alpha1.ExecGroupSource, argv []string, vars map[string]string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("the source defines no command for this lookup")
	}
	if r.runner == nil {
		return "", fmt.Errorf("group sources that run commands need a transport")
	}
	target := transport.Target{Name: src.Role, Role: src.Role}
	if r.target != nil {
		resolved, err := r.target(src.Role)
		if err != nil {
			return "", err
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

	result, err := r.runner.Run(r.ctx, target, transport.Request{Argv: command})
	if err != nil {
		return "", err
	}
	if result.Failed() {
		if result.Err != nil {
			return "", result.Err
		}
		return "", fmt.Errorf("%s exited %d: %s", command[0], result.ExitCode, strings.TrimSpace(result.Stderr))
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

// cacheKey identifies one lookup.
func cacheKey(source, kind, group string) string {
	return source + "\x00" + kind + "\x00" + group
}

func (r *Resolver) cached(source, kind, group string, src v1alpha1.GroupSource) (string, bool) {
	key := cacheKey(source, kind, group)

	r.mu.Lock()
	expr, ok := r.cache[key]
	r.mu.Unlock()
	if ok {
		return expr, true
	}
	ttl := src.CacheTTL.Get()
	if ttl <= 0 || r.cacheDir == "" {
		return "", false
	}

	path := r.cachePath(source, kind, group)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", false
	}
	if time.Since(entry.At) > ttl {
		return "", false
	}
	r.mu.Lock()
	r.cache[key] = entry.Expr
	r.mu.Unlock()
	return entry.Expr, true
}

func (r *Resolver) store(source, kind, group string, src v1alpha1.GroupSource, expr string) {
	r.mu.Lock()
	r.cache[cacheKey(source, kind, group)] = expr
	r.mu.Unlock()

	if src.CacheTTL.Get() <= 0 || r.cacheDir == "" {
		return
	}
	data, err := json.Marshal(cacheEntry{At: time.Now(), Expr: expr})
	if err != nil {
		return
	}
	// A cache that cannot be written is not worth failing a command over.
	_ = fileutil.WriteAtomic(r.cachePath(source, kind, group), data, 0o600)
}

type cacheEntry struct {
	At   time.Time `json:"at"`
	Expr string    `json:"expr"`
}

func (r *Resolver) cachePath(source, kind, group string) string {
	name := strings.NewReplacer("/", "_", "\x00", "_", " ", "_").Replace(source + "-" + kind + "-" + group)
	return filepath.Join(r.cacheDir, "groups", name+".json")
}
