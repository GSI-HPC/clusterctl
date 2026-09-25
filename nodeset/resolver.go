// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import (
	"cmp"
	"fmt"
	"sort"
	"strings"
)

// maxGroupDepth bounds how deeply groups may reference other groups, which
// also breaks reference cycles.
const maxGroupDepth = 16

// Resolver turns a group reference into a node set expression.
//
// A reference is written @group, or @source:group when several group sources
// are configured. The expression a resolver returns is parsed in turn, so a
// group may refer to other groups.
type Resolver interface {
	// Resolve returns the expression a group names. An unknown group is an
	// error; an empty group returns an empty expression.
	Resolve(source, group string) (string, error)
	// List returns the group names a source offers.
	List(source string) ([]string, error)
	// All returns the expression naming every host a source knows.
	All(source string) (string, error)
	// DefaultSource names the source used when a reference names none.
	DefaultSource() string
}

// resolveGroup evaluates one @reference.
func resolveGroup(ref string, res Resolver, depth int, b *budget) (*NodeSet, error) {
	if res == nil {
		return nil, fmt.Errorf("group @%s cannot be resolved: no group source is configured", ref)
	}
	// The source is passed on exactly as written, empty included, so that a
	// resolver backed by several sources can search them all for a bare
	// @group reference rather than being limited to its default.
	source, group := "", ref
	if before, after, ok := strings.Cut(ref, ":"); ok {
		source, group = before, after
	}
	if group == "" {
		return nil, fmt.Errorf("empty group name in @%s", ref)
	}

	var (
		expr string
		err  error
	)
	if group == "*" {
		expr, err = res.All(source)
	} else {
		expr, err = res.Resolve(source, group)
	}
	if err != nil {
		return nil, fmt.Errorf("group @%s: %w", ref, err)
	}
	if strings.TrimSpace(expr) == "" {
		return New(), nil
	}
	return parseExpression(expr, res, depth+1, b)
}

// MapResolver resolves groups from an in-memory table. It backs the static
// group definitions in the site configuration and the tests.
type MapResolver struct {
	// Groups maps a source name to its groups.
	Groups map[string]map[string]string
	// Default names the source used when a reference names none.
	Default string
}

// NewMapResolver builds a resolver for a single unnamed source.
func NewMapResolver(source string, groups map[string]string) *MapResolver {
	return &MapResolver{
		Groups:  map[string]map[string]string{source: groups},
		Default: source,
	}
}

// Resolve implements Resolver. An empty source means the default one.
func (m *MapResolver) Resolve(source, group string) (string, error) {
	source = cmp.Or(source, m.Default)
	groups, ok := m.Groups[source]
	if !ok {
		return "", fmt.Errorf("unknown group source %q", source)
	}
	expr, ok := groups[group]
	if !ok {
		return "", fmt.Errorf("unknown group %q in source %q", group, source)
	}
	return expr, nil
}

// List implements Resolver. An empty source means the default one.
func (m *MapResolver) List(source string) ([]string, error) {
	source = cmp.Or(source, m.Default)
	groups, ok := m.Groups[source]
	if !ok {
		return nil, fmt.Errorf("unknown group source %q", source)
	}
	out := make([]string, 0, len(groups))
	for name := range groups {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// All implements Resolver by unioning every group of the source.
func (m *MapResolver) All(source string) (string, error) {
	names, err := m.List(source)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		expr, err := m.Resolve(source, name)
		if err != nil {
			return "", err
		}
		parts = append(parts, expr)
	}
	return strings.Join(parts, ","), nil
}

// DefaultSource implements Resolver.
func (m *MapResolver) DefaultSource() string { return m.Default }
