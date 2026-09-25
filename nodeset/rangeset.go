// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// rangeSet is an ordered set of numbers forming one dimension of a host name.
//
// Padding is not part of a host's identity, so exe1 and exe01 are one host,
// but every value keeps the width it was written with: pads[i] is the width
// of values[i], and a set never shows a host under a name it was not given.
type rangeSet struct {
	values []int
	pads   []int
}

// padOf reports the display width a numeric literal asks for. Only a leading
// zero counts as padding, so "7" and "10" ask for none and "007" asks for
// three.
func padOf(lit string) int {
	if len(lit) > 1 && lit[0] == '0' {
		return len(lit)
	}
	return 0
}

// digits reports how many digits n has without padding.
func digits(n int) int {
	d := 1
	for ; n >= 10; n /= 10 {
		d++
	}
	return d
}

// normalPad drops a width that does not change how n is shown, so that each
// spelling of a number has exactly one width: 10 at width two is "10", the
// same as 10 at width zero.
func normalPad(n, pad int) int {
	if pad <= digits(n) {
		return 0
	}
	return pad
}

// format renders one number with its padding.
func format(n, pad int) string {
	if pad > 0 {
		return fmt.Sprintf("%0*d", pad, n)
	}
	return strconv.Itoa(n)
}

// fits reports whether a value shown with width pad reads the same when shown
// with width run, so that it can share a range with values of that width.
func fits(n, pad, run int) bool {
	return pad == run || (pad == 0 && digits(n) >= run)
}

// add appends one value with its width.
func (rs *rangeSet) add(n, pad int) {
	rs.values = append(rs.values, n)
	rs.pads = append(rs.pads, normalPad(n, pad))
}

// sortUnique orders the values and drops repeats. Of two spellings of one
// number, the one given first is kept.
func (rs *rangeSet) sortUnique() {
	idx := make([]int, len(rs.values))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return rs.values[idx[a]] < rs.values[idx[b]] })
	values := make([]int, 0, len(idx))
	pads := make([]int, 0, len(idx))
	for _, i := range idx {
		if n := len(values); n > 0 && values[n-1] == rs.values[i] {
			continue
		}
		values = append(values, rs.values[i])
		pads = append(pads, rs.pads[i])
	}
	rs.values, rs.pads = values, pads
}

// span is one part of a bracket expression, "1-10/2", before it is expanded.
type span struct {
	start, end, step, pad int
}

// count reports how many values the span expands to.
func (s span) count() int { return (s.end-s.start)/s.step + 1 }

// parseSpans reads the contents of a bracket expression, "1-10/2,20", without
// expanding it, and reports how many values it names in total. Every value
// counts, including repeats, so a range written as many parts weighs what one
// written as a single part does.
func parseSpans(spec string) ([]span, int, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, 0, fmt.Errorf("empty range")
	}
	var (
		spans []span
		total int
	)
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, 0, fmt.Errorf("empty range element in %q", spec)
		}
		step := 1
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			var err error
			step, err = parseNumber(part[slash+1:])
			if err != nil || step < 1 {
				return nil, 0, fmt.Errorf("invalid step %q in range %q", part[slash+1:], spec)
			}
			part = part[:slash]
		}
		lo, hi := part, ""
		if before, after, ok := strings.Cut(part, "-"); ok {
			lo, hi = before, after
		}
		start, err := parseNumber(lo)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid range bound %q in %q", lo, spec)
		}
		end := start
		if hi != "" {
			if end, err = parseNumber(hi); err != nil {
				return nil, 0, fmt.Errorf("invalid range bound %q in %q", hi, spec)
			}
			if end < start {
				return nil, 0, fmt.Errorf("descending range %q", part)
			}
			// The padding of the first bound applies to the whole range, so
			// a last bound written with another width would be shown under
			// a name it was not given: 1-010 would print 010 as 10.
			if (padOf(lo) > 0 && len(hi) < len(lo)) || (padOf(hi) > 0 && len(hi) != len(lo)) {
				return nil, 0, fmt.Errorf("the bounds of %q are padded to different widths", part)
			}
		} else if step != 1 {
			return nil, 0, fmt.Errorf("step given for the single value %q", part)
		}
		s := span{start: start, end: end, step: step, pad: padOf(lo)}
		spans = append(spans, s)
		// Both terms are at most maxRangeElements+1 here, so the sum cannot
		// overflow however many parts are written.
		if total += s.count(); total > maxRangeElements {
			return nil, 0, fmt.Errorf("range %q expands to more than %d elements", spec, maxRangeElements)
		}
	}
	return spans, total, nil
}

// expandSpans turns parsed spans into the dimension they name.
func expandSpans(spans []span, total int) *rangeSet {
	rs := &rangeSet{values: make([]int, 0, total), pads: make([]int, 0, total)}
	for _, s := range spans {
		for n := s.start; n <= s.end; n += s.step {
			rs.add(n, s.pad)
		}
	}
	rs.sortUnique()
	return rs
}

// parseNumber reads a decimal literal, rejecting anything else.
func parseNumber(lit string) (int, error) {
	if lit == "" || len(lit) > 18 {
		return 0, fmt.Errorf("invalid number %q", lit)
	}
	n := 0
	for i := 0; i < len(lit); i++ {
		if lit[i] < '0' || lit[i] > '9' {
			return 0, fmt.Errorf("invalid number %q", lit)
		}
		n = n*10 + int(lit[i]-'0')
	}
	return n, nil
}

// String renders the dimension. A single value is rendered bare so that it
// reads as part of the host name; anything else is bracketed.
func (rs *rangeSet) String(autostep int) string {
	if len(rs.values) == 1 {
		return format(rs.values[0], rs.pads[0])
	}
	return "[" + rs.list(autostep) + "]"
}

// list renders the values as comma separated ranges, without the brackets.
// A range only joins values that read the same at the width of its first
// value, so that parsing the output gives every value back its own width.
func (rs *rangeSet) list(autostep int) string {
	var parts []string
	vals, pads := rs.values, rs.pads
	for i := 0; i < len(vals); {
		run := pads[i]
		j := i + 1
		for j < len(vals) && vals[j] == vals[j-1]+1 && fits(vals[j], pads[j], run) {
			j++
		}
		if j-i >= 2 {
			parts = append(parts, format(vals[i], run)+"-"+format(vals[j-1], run))
			i = j
			continue
		}
		if autostep >= 2 {
			if k, step := rs.arithmeticRun(i); k-i >= autostep {
				parts = append(parts, fmt.Sprintf("%s-%s/%d",
					format(vals[i], run), format(vals[k-1], run), step))
				i = k
				continue
			}
		}
		parts = append(parts, format(vals[i], run))
		i++
	}
	return strings.Join(parts, ",")
}

// arithmeticRun finds the longest run starting at i with a constant step
// greater than one and values that read the same at the width of the first.
func (rs *rangeSet) arithmeticRun(i int) (end, step int) {
	vals, pads := rs.values, rs.pads
	if i+2 >= len(vals) || !fits(vals[i+1], pads[i+1], pads[i]) {
		return i, 0
	}
	if step = vals[i+1] - vals[i]; step < 2 {
		return i, 0
	}
	j := i + 2
	for j < len(vals) && vals[j]-vals[j-1] == step && fits(vals[j], pads[j], pads[i]) {
		j++
	}
	return j, step
}
