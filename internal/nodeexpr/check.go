// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package nodeexpr reads the node set expressions clusterctl resolves groups
// in.
//
// go-nodeset parses them. Before it does, this package refuses a group
// reference without a group name, @ or @rack:, which go-nodeset hands to the
// resolver. clusterctl's own parser refused it, with the error this package
// gives. Once go-nodeset refuses it itself, the check goes.
//
// The package also looks up the groups an expression names side by side,
// through a Batch, for a resolver whose every lookup is a round trip to a
// host.
package nodeexpr

import (
	"fmt"
	"strings"

	"github.com/GSI-HPC/go-nodeset"
)

// ParseWith is nodeset.ParseWith for an expression clusterctl was given. It
// refuses what check refuses, in the expression and in the expression of
// every group it names, at every level. When res is a BatchResolver, the
// groups of each level are looked up side by side through a Batch made for
// the expression; a Batch given as res is used as it is, with what it
// already found. Without a resolver it is nodeset.Parse, which refuses every
// group reference itself.
func ParseWith(expr string, res nodeset.Resolver) (*nodeset.NodeSet, error) {
	if res == nil {
		return nodeset.Parse(expr)
	}
	if err := check(expr); err != nil {
		return nil, err
	}
	if br, ok := res.(BatchResolver); ok {
		res = NewBatch(br)
	}
	if b, ok := res.(*Batch); ok {
		b.Prefetch(expr)
	}
	return nodeset.ParseWith(expr, checked{res})
}

// checked hands on what a resolver answers only once check has passed it, so
// that the expression of a group is held to what the expression naming it
// is held to.
type checked struct{ res nodeset.Resolver }

func (c checked) Resolve(source, group string) (string, error) {
	if group == "" {
		// check refuses the reference before it gets here; a resolver is
		// never asked for a group without a name.
		return "", fmt.Errorf("empty group name in @%s", reference(source, group))
	}
	expr, err := c.res.Resolve(source, group)
	if err != nil {
		return "", err
	}
	if err := check(expr); err != nil {
		return "", err
	}
	return expr, nil
}

func (c checked) All(source string) (string, error) {
	expr, err := c.res.All(source)
	if err != nil {
		return "", err
	}
	if err := check(expr); err != nil {
		return "", err
	}
	return expr, nil
}

// reference writes a group reference back as it is written in an
// expression.
func reference(source, group string) string {
	if source == "" {
		return group
	}
	return source + ":" + group
}

// check refuses a group reference without a group name, @ or @rack:, which
// go-nodeset reads but clusterctl does not. It is what @rack:$R leaves
// behind when R is empty. go-nodeset hands it to the resolver, and an
// attribute source answers it with every node that carries the attribute.
//
// The expression is read the way go-nodeset reads it, term by term between
// the operators and whitespace outside brackets. A term check cannot read is
// left to go-nodeset, which refuses it with its own error.
func check(expr string) error {
	depth, start := 0, -1
	for i := 0; i <= len(expr); i++ {
		c := byte(',') // the end of the expression ends its last term
		if i < len(expr) {
			c = expr[i]
		}
		switch {
		case c == '[':
			depth++
		case c == ']':
			if depth--; depth < 0 {
				return nil
			}
		case depth > 0:
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == '!' || c == '&' || c == '^':
			if start >= 0 {
				if err := checkTerm(strings.TrimSpace(expr[start:i])); err != nil {
					return err
				}
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	return nil
}

// checkTerm checks one term of an expression.
func checkTerm(term string) error {
	ref, ok := strings.CutPrefix(term, "@")
	if !ok {
		return nil
	}
	group := ref
	if _, after, ok := strings.Cut(ref, ":"); ok {
		group = after
	}
	if group == "" {
		return fmt.Errorf("empty group name in @%s", ref)
	}
	return nil
}
