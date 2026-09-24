// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import (
	"fmt"
	"strconv"
	"strings"
)

// The expansion limits, so that a typo such as exe[1-100000000] is reported
// before it exhausts memory. They are variables only so that tests can lower
// them.
var (
	// maxRangeElements caps how far a single bracket range may expand.
	maxRangeElements = 1 << 20
	// maxSetElements caps how many hosts a whole expression may name, at
	// every step of its evaluation, and also how many hosts all of its
	// terms may name together, groups included. The second cap bounds the
	// work an expression costs, which the first alone does not:
	// a[1-600000]!a[1-600000] repeated is small at every step.
	maxSetElements = 1 << 20
)

// budget is what is left of the hosts an expression may name across all its
// terms. It is shared by the groups the expression refers to.
type budget struct{ left int }

func newBudget() *budget { return &budget{left: maxSetElements} }

// node is one host: a pattern with a %s for each numeric dimension, the value
// of each dimension, and the width each value was written with. The widths
// are not part of the host's identity: exe1 and exe01 have one key.
type node struct {
	pattern string
	vals    []int
	pads    []int
}

// key identifies the host inside a NodeSet.
func (n node) key() string {
	var b strings.Builder
	b.WriteString(n.pattern)
	for _, v := range n.vals {
		b.WriteByte(0)
		b.WriteString(strconv.Itoa(v))
	}
	return b.String()
}

// name renders the host name as it was written.
func (n node) name() string {
	if len(n.vals) == 0 {
		return strings.ReplaceAll(n.pattern, "%%", "%")
	}
	args := make([]any, len(n.vals))
	for i, v := range n.vals {
		args[i] = format(v, n.pads[i])
	}
	return fmt.Sprintf(n.pattern, args...)
}

type operator byte

const (
	opUnion        operator = ','
	opDifference   operator = '!'
	opIntersection operator = '&'
	opSymmetric    operator = '^'
)

// parseExpression evaluates a full node set expression left to right.
//
// A comma or whitespace between two operands is a union, and an empty operand
// of a union is nothing: "a,,b" and "a," are accepted. The other operators
// need an operand on both sides. "a&" is what a command substitution that
// printed nothing leaves behind, and evaluating it as "a" would select every
// host of a instead of none, so it is an error, as it is in ClusterShell.
func parseExpression(expr string, res Resolver, depth int, b *budget) (*NodeSet, error) {
	if depth > maxGroupDepth {
		return nil, fmt.Errorf("group references nested more than %d levels deep", maxGroupDepth)
	}
	result := New()
	op := opUnion
	// operand reports whether the last thing read was an operand, and
	// pendingOp whether a set operator is waiting for its right operand.
	operand, pendingOp := false, false
	pending := strings.Builder{}
	depthBracket := 0

	flush := func() error {
		term := strings.TrimSpace(pending.String())
		pending.Reset()
		if term == "" {
			return nil
		}
		ts, err := parseTerm(term, res, depth, b)
		if err != nil {
			return err
		}
		if result.IsEmpty() && op == opUnion {
			// The first term needs no copy; the set is taken as it is.
			result = ts
		} else {
			apply(result, op, ts)
		}
		op, operand, pendingOp = opUnion, true, false
		if result.Len() > maxSetElements {
			return fmt.Errorf("%q expands to more than %d hosts", expr, maxSetElements)
		}
		return nil
	}

	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case c == '[':
			depthBracket++
			pending.WriteByte(c)
		case c == ']':
			depthBracket--
			if depthBracket < 0 {
				return nil, fmt.Errorf("unbalanced ] in %q", expr)
			}
			pending.WriteByte(c)
		case depthBracket > 0:
			pending.WriteByte(c)
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if err := flush(); err != nil {
				return nil, err
			}
		case c == byte(opUnion):
			if err := flush(); err != nil {
				return nil, err
			}
			if pendingOp {
				return nil, missingOperand(expr, op, "right")
			}
			operand = false
		case c == byte(opDifference) || c == byte(opIntersection) || c == byte(opSymmetric):
			if err := flush(); err != nil {
				return nil, err
			}
			if pendingOp {
				return nil, missingOperand(expr, op, "right")
			}
			if !operand {
				return nil, missingOperand(expr, operator(c), "left")
			}
			op, operand, pendingOp = operator(c), false, true
		default:
			pending.WriteByte(c)
		}
	}
	if depthBracket != 0 {
		return nil, fmt.Errorf("unbalanced [ in %q", expr)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if pendingOp {
		return nil, missingOperand(expr, op, "right")
	}
	return result, nil
}

// missingOperand reports an operator written without one of its operands.
func missingOperand(expr string, op operator, side string) error {
	return fmt.Errorf("in %q: the %c operator has no %s operand", expr, op, side)
}

func apply(dst *NodeSet, op operator, src *NodeSet) {
	switch op {
	case opDifference:
		dst.subtract(src)
	case opIntersection:
		dst.intersect(src)
	case opSymmetric:
		dst.symmetricDifference(src)
	default:
		dst.merge(src)
	}
}

// parseTerm parses a single term: either a group reference or a node pattern.
func parseTerm(term string, res Resolver, depth int, b *budget) (*NodeSet, error) {
	if strings.HasPrefix(term, "@") {
		return resolveGroup(term[1:], res, depth, b)
	}
	return parsePattern(term, b)
}

// parsePattern turns one node pattern into the hosts it names, charging them
// to b. Every dimension is weighed before it is expanded, against what the
// dimensions before it leave of the budget, so an oversized pattern is
// refused before its memory is spent.
func parsePattern(term string, b *budget) (*NodeSet, error) {
	var (
		pattern strings.Builder
		dims    []*rangeSet
		// Two numeric parts with nothing between them cannot be told apart
		// again once expanded: exe0[0,10] would print exe00, which reads as
		// a single number. Such a name is rejected rather than folded wrong.
		prevNumeric bool
		// total is the number of hosts the dimensions read so far name.
		total = 1
	)
	tooMany := func() error {
		if b.left < maxSetElements {
			return fmt.Errorf("at %q: the expression's terms name more than %d hosts together", term, maxSetElements)
		}
		return fmt.Errorf("%q expands to more than %d hosts", term, maxSetElements)
	}
	for i := 0; i < len(term); {
		c := term[i]
		numeric := c == '[' || (c >= '0' && c <= '9')
		if numeric && prevNumeric {
			return nil, fmt.Errorf("in %q: two numeric parts are adjacent at offset %d; separate them with a literal character", term, i)
		}
		prevNumeric = numeric

		switch {
		case c == '[':
			end := strings.IndexByte(term[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unbalanced [ in %q", term)
			}
			spans, count, err := parseSpans(term[i+1 : i+end])
			if err != nil {
				return nil, fmt.Errorf("in %q: %w", term, err)
			}
			if count > b.left/total {
				return nil, tooMany()
			}
			total *= count
			dims = append(dims, expandSpans(spans, count))
			pattern.WriteString("%s")
			i += end + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(term) && term[j] >= '0' && term[j] <= '9' {
				j++
			}
			lit := term[i:j]
			n, err := parseNumber(lit)
			if err != nil {
				return nil, fmt.Errorf("in %q: %w", term, err)
			}
			rs := &rangeSet{}
			rs.add(n, padOf(lit))
			dims = append(dims, rs)
			pattern.WriteString("%s")
			i = j
		case c == ']':
			return nil, fmt.Errorf("unbalanced ] in %q", term)
		case c == '%':
			pattern.WriteString("%%")
			i++
		default:
			pattern.WriteByte(c)
			i++
		}
	}
	if pattern.Len() == 0 && len(dims) == 0 {
		return nil, fmt.Errorf("empty node name")
	}
	if total > b.left {
		return nil, tooMany()
	}
	b.left -= total

	ns := &NodeSet{nodes: make(map[string]node, total)}
	pat := pattern.String()
	width := len(dims)
	vals := make([]int, width)
	pads := make([]int, width)
	// The hosts share two backing arrays rather than holding two small
	// slices each, which halves what a large set costs.
	allVals := make([]int, 0, total*width)
	allPads := make([]int, 0, total*width)
	var walk func(int)
	walk = func(d int) {
		if d == len(dims) {
			allVals = append(allVals, vals...)
			allPads = append(allPads, pads...)
			end := len(allVals)
			n := node{pattern: pat, vals: allVals[end-width : end : end], pads: allPads[end-width : end : end]}
			ns.nodes[n.key()] = n
			return
		}
		for i, v := range dims[d].values {
			vals[d], pads[d] = v, dims[d].pads[i]
			walk(d + 1)
		}
	}
	walk(0)
	return ns, nil
}
