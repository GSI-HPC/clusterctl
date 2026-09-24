// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import (
	"sort"
	"strings"
)

// NodeSet is an unordered set of host names that renders in folded form.
// The zero value is not usable; call New or Parse.
//
// Padding is not part of a host's identity: exe1 and exe01 are one host. Each
// host keeps the spelling it was first given, and when sets are combined the
// spelling already held wins, so a set never shows a host under a name it was
// not given.
type NodeSet struct {
	nodes    map[string]node
	autostep int
}

// Option configures parsing and rendering.
type Option func(*NodeSet)

// WithAutostep folds arithmetic progressions of at least n elements into
// "first-last/step" form. ClusterShell disables this by default and so does
// clusterctl; n below 2 keeps it disabled.
func WithAutostep(n int) Option {
	return func(ns *NodeSet) { ns.autostep = n }
}

// New returns an empty set.
func New(opts ...Option) *NodeSet {
	ns := &NodeSet{nodes: make(map[string]node)}
	for _, o := range opts {
		o(ns)
	}
	return ns
}

// Parse evaluates a node set expression without resolving groups. An
// expression containing a group reference is rejected.
func Parse(expr string, opts ...Option) (*NodeSet, error) {
	return ParseWith(expr, nil, opts...)
}

// ParseWith evaluates a node set expression, resolving group references
// through res. A nil resolver rejects every group reference.
func ParseWith(expr string, res Resolver, opts ...Option) (*NodeSet, error) {
	ns, err := parseExpression(expr, res, 0, newBudget())
	if err != nil {
		return nil, err
	}
	for _, o := range opts {
		o(ns)
	}
	return ns, nil
}

// MustParse is Parse for expressions fixed at compile time; it panics on a
// parse error.
func MustParse(expr string, opts ...Option) *NodeSet {
	ns, err := Parse(expr, opts...)
	if err != nil {
		panic(err)
	}
	return ns
}

// Len reports the number of hosts in the set.
func (ns *NodeSet) Len() int { return len(ns.nodes) }

// IsEmpty reports whether the set names no host.
func (ns *NodeSet) IsEmpty() bool { return len(ns.nodes) == 0 }

// Clone returns an independent copy.
func (ns *NodeSet) Clone() *NodeSet {
	out := &NodeSet{nodes: make(map[string]node, len(ns.nodes)), autostep: ns.autostep}
	for k, v := range ns.nodes {
		out.nodes[k] = v
	}
	return out
}

// Add parses names and adds the hosts they name to the set.
func (ns *NodeSet) Add(expr string) error {
	other, err := parseExpression(expr, nil, 0, newBudget())
	if err != nil {
		return err
	}
	ns.merge(other)
	return nil
}

// Contains reports whether name is a member. Padding is ignored, so "exe01"
// and "exe1" name the same host.
func (ns *NodeSet) Contains(name string) bool {
	_, ok := ns.lookup(name)
	return ok
}

// Canonical returns the name this set holds for a host, which is always a name
// the set was given.
//
// It differs from the name asked about when the two were written with
// different padding: a set holding exe0001 answers "exe0001" when asked about
// "exe1", because padding is not part of a host's identity and both name one
// host. A set holding exe0001 and exe11 answers "exe11" for exe11.
func (ns *NodeSet) Canonical(name string) (string, bool) {
	n, ok := ns.lookup(name)
	if !ok {
		return "", false
	}
	return n.name(), true
}

// lookup finds the member a single host name refers to.
func (ns *NodeSet) lookup(name string) (node, bool) {
	other, err := parsePattern(name, newBudget())
	if err != nil || len(other.nodes) != 1 {
		return node{}, false
	}
	for k := range other.nodes {
		n, ok := ns.nodes[k]
		return n, ok
	}
	return node{}, false
}

// Expand returns the host names in ascending order: by pattern first, then by
// each numeric dimension from left to right.
func (ns *NodeSet) Expand() []string {
	nodes := ns.sorted()
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.name()
	}
	return out
}

// sorted returns the members in expansion order.
func (ns *NodeSet) sorted() []node {
	nodes := make([]node, 0, len(ns.nodes))
	for _, n := range ns.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return lessNode(nodes[i], nodes[j]) })
	return nodes
}

// String renders the set in folded form, the representation clusterctl prints
// and accepts. An empty set renders as the empty string.
func (ns *NodeSet) String() string { return ns.fold() }

// Union returns the hosts in either set.
func (ns *NodeSet) Union(other *NodeSet) *NodeSet {
	out := ns.Clone()
	out.merge(other)
	return out
}

// Intersection returns the hosts in both sets.
func (ns *NodeSet) Intersection(other *NodeSet) *NodeSet {
	out := ns.Clone()
	out.intersect(other)
	return out
}

// Difference returns the hosts of ns that are not in other.
func (ns *NodeSet) Difference(other *NodeSet) *NodeSet {
	out := ns.Clone()
	out.subtract(other)
	return out
}

// SymmetricDifference returns the hosts in exactly one of the two sets.
func (ns *NodeSet) SymmetricDifference(other *NodeSet) *NodeSet {
	out := ns.Clone()
	out.symmetricDifference(other)
	return out
}

// Split partitions the set into at most n chunks of near equal size, in
// expansion order. It returns nil for n below one.
func (ns *NodeSet) Split(n int) []*NodeSet {
	if n < 1 {
		return nil
	}
	nodes := ns.sorted()
	if len(nodes) == 0 {
		return nil
	}
	if n > len(nodes) {
		n = len(nodes)
	}
	out := make([]*NodeSet, 0, n)
	size, rest := len(nodes)/n, len(nodes)%n
	for i := 0; i < n; i++ {
		take := size
		if i < rest {
			take++
		}
		chunk := &NodeSet{nodes: make(map[string]node, take), autostep: ns.autostep}
		for _, m := range nodes[:take] {
			chunk.nodes[m.key()] = m
		}
		nodes = nodes[take:]
		out = append(out, chunk)
	}
	return out
}

// merge adds the members of other. A host this set already holds keeps the
// spelling it has.
func (ns *NodeSet) merge(other *NodeSet) {
	for k, v := range other.nodes {
		if _, ok := ns.nodes[k]; !ok {
			ns.nodes[k] = v
		}
	}
}

func (ns *NodeSet) subtract(other *NodeSet) {
	for k := range other.nodes {
		delete(ns.nodes, k)
	}
}

func (ns *NodeSet) intersect(other *NodeSet) {
	for k := range ns.nodes {
		if _, ok := other.nodes[k]; !ok {
			delete(ns.nodes, k)
		}
	}
}

func (ns *NodeSet) symmetricDifference(other *NodeSet) {
	for k, v := range other.nodes {
		if _, ok := ns.nodes[k]; ok {
			delete(ns.nodes, k)
		} else {
			ns.nodes[k] = v
		}
	}
}

// lessNode orders hosts by pattern, then numerically by dimension, so that
// exe2 sorts before exe10.
func lessNode(a, b node) bool {
	if a.pattern != b.pattern {
		return a.pattern < b.pattern
	}
	for i := range a.vals {
		if i >= len(b.vals) {
			return false
		}
		if a.vals[i] != b.vals[i] {
			return a.vals[i] < b.vals[i]
		}
	}
	return len(a.vals) < len(b.vals)
}

// Hostlist renders the set in the syntax Slurm and FreeIPMI accept. Those
// parsers understand a single bracketed range per name but not several
// dimensions, so a multi dimensional set is expanded instead of folded.
func (ns *NodeSet) Hostlist() string {
	for _, n := range ns.nodes {
		if len(n.vals) > 1 {
			return strings.Join(ns.Expand(), ",")
		}
	}
	return ns.fold()
}
