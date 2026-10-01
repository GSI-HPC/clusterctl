// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import (
	"strings"
	"sync"
)

// BatchResolver is a Resolver that can look up several groups side by side,
// as one whose every lookup is a round trip to a remote source would rather
// do than one after the other. An expression that names several groups has
// them looked up together before it is evaluated; the evaluation then takes
// each answer in turn, as it would have asked for it.
type BatchResolver interface {
	Resolver
	// ResolveAll looks up the groups refs name and returns, in their
	// order, what Resolve returns for each. A group it did not get to, as
	// when it was interrupted, has no answer: a nil slice, or one shorter
	// than refs, leaves the rest to Resolve.
	ResolveAll(refs []GroupRef) []GroupAnswer
}

// GroupRef is a group reference, @Source:Group, or @Group with no Source.
type GroupRef struct {
	Source, Group string
}

// GroupAnswer is what a resolver answered for a group: the expression it
// names, or why it names none.
type GroupAnswer struct {
	Expr string
	Err  error
}

// Batch is a Resolver that looks up the groups expressions name through a
// BatchResolver, side by side, and answers from what it found for as long
// as it is kept, failures included: a failure a resolver does not remember,
// as it should not remember one that may not last, would otherwise cost a
// second lookup when the expression is evaluated, which could even answer
// otherwise. ParseWith keeps one for each expression it is given a
// BatchResolver for; a program that parses several expressions that may
// name the same groups keeps one across them. It is safe for concurrent
// use.
type Batch struct {
	res BatchResolver

	mu      sync.Mutex
	answers map[GroupRef]GroupAnswer
}

// NewBatch returns a Batch that looks up groups through res.
func NewBatch(res BatchResolver) *Batch {
	return &Batch{res: res, answers: map[GroupRef]GroupAnswer{}}
}

// Prefetch looks up, side by side, the groups the expressions name at their
// top level that it has no answer for yet. Groups named inside a group are
// looked up as its expression is evaluated, the groups of each level
// together. An expression that names one group has nothing to look up side
// by side and is left to the evaluation.
func (b *Batch) Prefetch(exprs ...string) {
	var refs []GroupRef
	seen := map[GroupRef]bool{}
	b.mu.Lock()
	for _, expr := range exprs {
		for _, ref := range groupRefs(expr) {
			if _, answered := b.answers[ref]; !answered && !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
	}
	b.mu.Unlock()
	if len(refs) < 2 {
		return
	}
	answers := b.res.ResolveAll(refs)
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, answer := range answers[:min(len(answers), len(refs))] {
		b.answers[refs[i]] = answer
	}
}

// Resolve implements Resolver, from what Prefetch found when it looked the
// group up, and otherwise through the BatchResolver.
func (b *Batch) Resolve(source, group string) (string, error) {
	b.mu.Lock()
	answer, ok := b.answers[GroupRef{Source: source, Group: group}]
	b.mu.Unlock()
	if ok {
		return answer.Expr, answer.Err
	}
	return b.res.Resolve(source, group)
}

// All implements Resolver through the BatchResolver.
func (b *Batch) All(source string) (string, error) { return b.res.All(source) }

// groupRefs returns the group references an expression names at its top
// level, read the way parseExpression reads its terms. @source:* and a
// reference without a group name are left out: the first is All's, and the
// second is an error the evaluation reports.
func groupRefs(expr string) []GroupRef {
	var (
		refs  []GroupRef
		start = -1
		depth = 0
	)
	term := func(end int) {
		if start < 0 {
			return
		}
		t := strings.TrimSpace(expr[start:end])
		start = -1
		ref, ok := strings.CutPrefix(t, "@")
		if !ok {
			return
		}
		source, group := "", ref
		if before, after, ok := strings.Cut(ref, ":"); ok {
			source, group = before, after
		}
		if group != "" && group != "*" {
			refs = append(refs, GroupRef{Source: source, Group: group})
		}
	}
	for i := 0; i < len(expr); i++ {
		switch c := expr[i]; {
		case c == '[':
			depth++
		case c == ']':
			depth--
		case depth > 0:
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' ||
			c == byte(opUnion) || c == byte(opDifference) || c == byte(opIntersection) || c == byte(opSymmetric):
			term(i)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	term(len(expr))
	return refs
}
