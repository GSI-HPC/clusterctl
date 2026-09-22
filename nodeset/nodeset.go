// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import (
	"sort"
	"strings"
)

// NodeSet is an unordered set of host names that renders in folded form.
// The zero value is not usable; call New or Parse.
type NodeSet struct {
	nodes map[string]node
	// pads records the display width of each dimension of a pattern. When
	// sets are combined the padding already recorded wins, so exe[01-02]
	// keeps its width when exe3 is added to it.
	pads     map[string][]int
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
	ns := &NodeSet{nodes: make(map[string]node), pads: make(map[string][]int)}
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
	ns, err := parseExpression(expr, res, 0)
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
	out := &NodeSet{
		nodes:    make(map[string]node, len(ns.nodes)),
		pads:     make(map[string][]int, len(ns.pads)),
		autostep: ns.autostep,
	}
	for k, v := range ns.nodes {
		out.nodes[k] = v
	}
	for k, v := range ns.pads {
		out.pads[k] = append([]int(nil), v...)
	}
	return out
}

// Add parses names and adds the hosts they name to the set.
func (ns *NodeSet) Add(expr string) error {
	other, err := parseExpression(expr, nil, 0)
	if err != nil {
		return err
	}
	ns.merge(other)
	return nil
}

// Contains reports whether name is a member. Padding is ignored, so "exe01"
// and "exe1" name the same host.
func (ns *NodeSet) Contains(name string) bool {
	other, err := parsePattern(name)
	if err != nil || len(other.nodes) != 1 {
		return false
	}
	for k := range other.nodes {
		if _, ok := ns.nodes[k]; ok {
			return true
		}
	}
	return false
}

// Expand returns the host names in ascending order: by pattern first, then by
// each numeric dimension from left to right.
func (ns *NodeSet) Expand() []string {
	nodes := make([]node, 0, len(ns.nodes))
	for _, n := range ns.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return lessNode(nodes[i], nodes[j]) })
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.name(ns.pads[n.pattern])
	}
	return out
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
	names := ns.Expand()
	if len(names) == 0 {
		return nil
	}
	if n > len(names) {
		n = len(names)
	}
	out := make([]*NodeSet, 0, n)
	size, rest := len(names)/n, len(names)%n
	for i := 0; i < n; i++ {
		take := size
		if i < rest {
			take++
		}
		chunk := &NodeSet{
			nodes:    make(map[string]node, take),
			pads:     make(map[string][]int, len(ns.pads)),
			autostep: ns.autostep,
		}
		for pattern, pads := range ns.pads {
			chunk.pads[pattern] = append([]int(nil), pads...)
		}
		for _, name := range names[:take] {
			if err := chunk.Add(name); err != nil {
				panic("nodeset: re-parsing an expanded host failed: " + err.Error())
			}
		}
		names = names[take:]
		out = append(out, chunk)
	}
	return out
}

func (ns *NodeSet) merge(other *NodeSet) {
	for k, v := range other.nodes {
		ns.nodes[k] = v
	}
	ns.adoptPads(other)
}

// adoptPads takes over the display widths of patterns this set does not
// already show, and of dimensions it shows without padding.
func (ns *NodeSet) adoptPads(other *NodeSet) {
	for pattern, pads := range other.pads {
		mine, ok := ns.pads[pattern]
		if !ok {
			ns.pads[pattern] = append([]int(nil), pads...)
			continue
		}
		for i := range mine {
			if mine[i] == 0 && i < len(pads) {
				mine[i] = pads[i]
			}
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
	ns.adoptPads(other)
}

// lessNode orders hosts by pattern, then numerically by dimension, so that
// exe2 sorts before exe10 and exe1 before exe01.
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
