// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package nodeexpr reads the node set expressions clusterctl is given.
//
// go-nodeset parses them. Before it does, this package refuses two kinds of
// expression that go-nodeset v1.0.0 reads but a selection must not: a range
// without its last bound, which go-nodeset reads as its first value alone
// (exe[1-] as exe1), and a group reference without a group name, @ or
// @rack:, which go-nodeset hands to the resolver. clusterctl's own parser
// refused both, with the errors this package gives. Once go-nodeset refuses
// them itself, the check goes, and Parse, ParseWith and Add with it.
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

// Parse is nodeset.Parse for an expression clusterctl was given: it refuses
// what check refuses as well.
func Parse(expr string) (*nodeset.NodeSet, error) {
	if err := check(expr, false); err != nil {
		return nil, err
	}
	return nodeset.Parse(expr)
}

// ParseWith is nodeset.ParseWith for an expression clusterctl was given. It
// refuses what check refuses, in the expression and in the expression of
// every group it names, at every level. When res is a BatchResolver, the
// groups of each level are looked up side by side through a Batch made for
// the expression; a Batch given as res is used as it is, with what it
// already found.
func ParseWith(expr string, res nodeset.Resolver) (*nodeset.NodeSet, error) {
	if res == nil {
		return Parse(expr)
	}
	if err := check(expr, true); err != nil {
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

// Add is NodeSet.Add for names clusterctl was given: it refuses what check
// refuses as well.
func Add(ns *nodeset.NodeSet, expr string) error {
	if err := check(expr, false); err != nil {
		return err
	}
	return ns.Add(expr)
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
	if err := check(expr, true); err != nil {
		return "", err
	}
	return expr, nil
}

func (c checked) All(source string) (string, error) {
	expr, err := c.res.All(source)
	if err != nil {
		return "", err
	}
	if err := check(expr, true); err != nil {
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

// check refuses two things go-nodeset reads but clusterctl does not.
//
// A range without its last bound, such as exe[1-] or exe[1-,5], is what
// exe[1-$N] leaves behind when N is empty. go-nodeset reads it as its first
// value alone, which would run a command meant for many nodes on one.
//
// A group reference without a group name, @ or @rack:, is what @rack:$R
// leaves behind when R is empty. go-nodeset hands it to the resolver, and an
// attribute source answers it with every node that carries the attribute.
// It is checked only where groups are resolved: without a resolver
// go-nodeset refuses every reference itself.
//
// The expression is read the way go-nodeset reads it, term by term between
// the operators and whitespace outside brackets. A term check cannot read is
// left to go-nodeset, which refuses it with its own error.
func check(expr string, groups bool) error {
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
				if err := checkTerm(strings.TrimSpace(expr[start:i]), groups); err != nil {
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
func checkTerm(term string, groups bool) error {
	if ref, ok := strings.CutPrefix(term, "@"); ok {
		group := ref
		if _, after, ok := strings.Cut(ref, ":"); ok {
			group = after
		}
		if groups && group == "" {
			return fmt.Errorf("empty group name in @%s", ref)
		}
		return nil
	}
	if strings.HasPrefix(term, "-") {
		// Not a host name, which go-nodeset says.
		return nil
	}
	for i := 0; i < len(term); i++ {
		if term[i] != '[' {
			continue
		}
		if i > 0 && (term[i-1] == ']' || term[i-1] >= '0' && term[i-1] <= '9') {
			// Two numeric parts are adjacent, which go-nodeset says.
			return nil
		}
		end := strings.IndexByte(term[i:], ']')
		if end < 0 {
			return nil
		}
		if part := unbounded(term[i+1 : i+end]); part != "" {
			return fmt.Errorf("in %q: the range %q has no last bound", term, part)
		}
		i += end
	}
	return nil
}

// unbounded returns the first range of a bracket, "1-10/2,20", that has its
// first bound and a dash but no last bound, without its step, or "" when
// there is none. It stops at a range it cannot read, which go-nodeset
// refuses for what it is.
func unbounded(spec string) string {
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if before, step, ok := strings.Cut(part, "/"); ok {
			if !isNumber(step) || strings.Trim(step, "0") == "" {
				return ""
			}
			part = before
		}
		first, last, isRange := strings.Cut(part, "-")
		switch {
		case !isNumber(first):
			return ""
		case isRange && last == "":
			return part
		case isRange && !isNumber(last):
			return ""
		}
	}
	return ""
}

// isNumber reports whether s is a number as a bound or a step is written:
// one to eighteen decimal digits.
func isNumber(s string) bool {
	if s == "" || len(s) > 18 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
