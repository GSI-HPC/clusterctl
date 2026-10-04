// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package nodeexpr looks up the groups a node set expression names side by
// side, for a resolver whose every lookup is a round trip to a host.
//
// go-nodeset resolves the group references of an expression one after the
// other, as it evaluates them. ParseWith has the groups of each level of
// nesting looked up together first, through a Batch, and go-nodeset then
// takes each answer in turn. Once go-nodeset looks up the groups of a level
// together itself, the package goes.
package nodeexpr

import "github.com/GSI-HPC/go-nodeset"

// ParseWith is nodeset.ParseWith with the groups of each level of nesting
// looked up side by side. When res is a BatchResolver, it is asked through a
// Batch made for the expression; a Batch given as res is used as it is, with
// what it already found. Any other resolver is asked as nodeset.ParseWith
// asks it.
func ParseWith(expr string, res nodeset.Resolver) (*nodeset.NodeSet, error) {
	if br, ok := res.(BatchResolver); ok {
		res = NewBatch(br)
	}
	if b, ok := res.(*Batch); ok {
		b.Prefetch(expr)
	}
	return nodeset.ParseWith(expr, res)
}
