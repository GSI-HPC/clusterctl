// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import (
	"fmt"
	"sort"
	"strings"
)

// vector is one folded host name: a pattern and the set of values each of its
// dimensions takes.
type vector struct {
	pattern string
	dims    []*rangeSet
}

// patternNodes is the members of a set that share one pattern.
type patternNodes struct {
	pattern string
	nodes   []node
}

// byPattern groups the members by pattern, in pattern order.
func (ns *NodeSet) byPattern() []patternNodes {
	groups := make(map[string][]node)
	for _, n := range ns.nodes {
		groups[n.pattern] = append(groups[n.pattern], n)
	}
	out := make([]patternNodes, 0, len(groups))
	for p, nodes := range groups {
		out = append(out, patternNodes{pattern: p, nodes: nodes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pattern < out[j].pattern })
	return out
}

// fold renders the set, merging hosts into bracketed ranges.
func (ns *NodeSet) fold() string {
	var parts []string
	for _, p := range ns.byPattern() {
		for _, v := range foldPattern(p.pattern, p.nodes, ns.autostep) {
			parts = append(parts, v.render(ns.autostep))
		}
	}
	return strings.Join(parts, ",")
}

// unitVectors makes one vector per host, each dimension holding the host's
// value with the width it was written with.
func unitVectors(pattern string, nodes []node) []vector {
	vectors := make([]vector, 0, len(nodes))
	for _, n := range nodes {
		dims := make([]*rangeSet, len(n.vals))
		for i, v := range n.vals {
			dims[i] = &rangeSet{values: []int{v}, pads: []int{n.pads[i]}}
		}
		vectors = append(vectors, vector{pattern: pattern, dims: dims})
	}
	return vectors
}

// foldPattern merges the hosts of one pattern into as few vectors as it can.
// Each pass picks one dimension and unions it across vectors whose other
// dimensions are identical, which is repeated until nothing merges any more.
func foldPattern(pattern string, nodes []node, autostep int) []vector {
	vectors := unitVectors(pattern, nodes)
	if len(vectors) < 2 || len(vectors[0].dims) == 0 {
		sortVectors(vectors)
		return vectors
	}

	for changed := true; changed; {
		changed = false
		for axis := range vectors[0].dims {
			merged, did := foldAxis(vectors, axis, autostep)
			vectors = merged
			changed = changed || did
		}
	}
	sortVectors(vectors)
	return vectors
}

// foldAxis unions the values of one dimension across every pair of vectors
// that agree on all other dimensions.
//
// Values are collected first and put in order once per merged vector, so a
// pass costs O(n log n) however many vectors fold into one.
func foldAxis(vectors []vector, axis, autostep int) ([]vector, bool) {
	groups := make(map[string]int, len(vectors))
	out := make([]vector, 0, len(vectors))
	grown := make([]bool, 0, len(vectors))
	changed := false

	for _, v := range vectors {
		key := v.keyWithout(axis, autostep)
		if idx, ok := groups[key]; ok {
			target := out[idx].dims[axis]
			target.values = append(target.values, v.dims[axis].values...)
			target.pads = append(target.pads, v.dims[axis].pads...)
			grown[idx] = true
			changed = true
			continue
		}
		groups[key] = len(out)
		out = append(out, v)
		grown = append(grown, false)
	}
	for idx, g := range grown {
		if g {
			out[idx].dims[axis].sortUnique()
		}
	}
	return out, changed
}

// keyWithout identifies a vector by every dimension except axis.
func (v vector) keyWithout(axis, autostep int) string {
	var b strings.Builder
	for i, d := range v.dims {
		if i == axis {
			b.WriteString("\x00*\x00")
			continue
		}
		b.WriteString(d.String(autostep))
		b.WriteByte(0)
	}
	return b.String()
}

// render fills the pattern with the rendered dimensions.
func (v vector) render(autostep int) string {
	if len(v.dims) == 0 {
		return strings.ReplaceAll(v.pattern, "%%", "%")
	}
	args := make([]any, len(v.dims))
	for i, d := range v.dims {
		args[i] = d.String(autostep)
	}
	return fmt.Sprintf(v.pattern, args...)
}

// sortVectors orders folded names by their lowest value in each dimension, so
// that output is deterministic.
func sortVectors(vectors []vector) {
	sort.Slice(vectors, func(i, j int) bool {
		a, b := vectors[i], vectors[j]
		if a.pattern != b.pattern {
			return a.pattern < b.pattern
		}
		for d := range a.dims {
			if d >= len(b.dims) {
				return false
			}
			if av, bv := a.dims[d].values[0], b.dims[d].values[0]; av != bv {
				return av < bv
			}
		}
		return false
	})
}
