// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// rangeSet is an ordered set of numbers forming one dimension of a host name,
// together with the zero padding the dimension is displayed with.
//
// Padding is a display property, as it is in ClusterShell: exe1 and exe01 are
// the same host, shown with a width of one or two digits.
type rangeSet struct {
	values []int
	pad    int
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

// format renders one number with the padding of the dimension.
func format(n, pad int) string {
	if pad > 0 {
		return fmt.Sprintf("%0*d", pad, n)
	}
	return strconv.Itoa(n)
}

func (rs *rangeSet) sortUnique() {
	sort.Ints(rs.values)
	out := rs.values[:0]
	for i, v := range rs.values {
		if i == 0 || v != rs.values[i-1] {
			out = append(out, v)
		}
	}
	rs.values = out
}

// parseRangeSet reads the contents of a bracket expression, "1-10/2,20". The
// padding of the whole dimension is the first one any bound asks for.
func parseRangeSet(spec string) (*rangeSet, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("empty range")
	}
	rs := &rangeSet{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty range element in %q", spec)
		}
		step := 1
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			var err error
			step, err = strconv.Atoi(part[slash+1:])
			if err != nil || step < 1 {
				return nil, fmt.Errorf("invalid step %q in range %q", part[slash+1:], spec)
			}
			part = part[:slash]
		}
		lo, hi := part, ""
		if dash := strings.IndexByte(part, '-'); dash >= 0 {
			lo, hi = part[:dash], part[dash+1:]
		}
		start, err := parseNumber(lo)
		if err != nil {
			return nil, fmt.Errorf("invalid range bound %q in %q", lo, spec)
		}
		if rs.pad == 0 {
			rs.pad = padOf(lo)
		}
		end := start
		if hi != "" {
			if end, err = parseNumber(hi); err != nil {
				return nil, fmt.Errorf("invalid range bound %q in %q", hi, spec)
			}
			if end < start {
				return nil, fmt.Errorf("descending range %q", part)
			}
		} else if step != 1 {
			return nil, fmt.Errorf("step given for the single value %q", part)
		}
		// Every element counts, including those of earlier parts, so a range
		// written as many parts is capped like one written as a single part.
		if len(rs.values)+(end-start)/step+1 > maxRangeElements {
			return nil, fmt.Errorf("range %q expands to more than %d elements", spec, maxRangeElements)
		}
		for n := start; n <= end; n += step {
			rs.values = append(rs.values, n)
		}
	}
	rs.sortUnique()
	return rs, nil
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
		return format(rs.values[0], rs.pad)
	}
	return "[" + rs.list(autostep) + "]"
}

// list renders the values as comma separated ranges, without the brackets.
func (rs *rangeSet) list(autostep int) string {
	var parts []string
	vals := rs.values
	for i := 0; i < len(vals); {
		j := i + 1
		for j < len(vals) && vals[j] == vals[j-1]+1 {
			j++
		}
		if j-i >= 2 {
			parts = append(parts, format(vals[i], rs.pad)+"-"+format(vals[j-1], rs.pad))
			i = j
			continue
		}
		if autostep >= 2 {
			if k, step := arithmeticRun(vals, i); k-i >= autostep {
				parts = append(parts, fmt.Sprintf("%s-%s/%d",
					format(vals[i], rs.pad), format(vals[k-1], rs.pad), step))
				i = k
				continue
			}
		}
		parts = append(parts, format(vals[i], rs.pad))
		i++
	}
	return strings.Join(parts, ",")
}

// arithmeticRun finds the longest run starting at i with a constant step
// greater than one.
func arithmeticRun(vals []int, i int) (end, step int) {
	if i+2 >= len(vals) {
		return i, 0
	}
	if step = vals[i+1] - vals[i]; step < 2 {
		return i, 0
	}
	j := i + 2
	for j < len(vals) && vals[j]-vals[j-1] == step {
		j++
	}
	return j, step
}
