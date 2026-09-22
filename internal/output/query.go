// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"
)

// writeJQ filters the result with a jq program. The jq language is embedded,
// so no jq binary has to be installed.
func writeJQ(w io.Writer, program string, v any) error {
	query, err := gojq.Parse(program)
	if err != nil {
		return fmt.Errorf("invalid jq expression %q: %w", program, err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return fmt.Errorf("invalid jq expression %q: %w", program, err)
	}
	input, err := toGeneric(v)
	if err != nil {
		return err
	}
	iter := code.Run(input)
	for {
		out, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, ok := out.(error); ok {
			return fmt.Errorf("jq: %w", err)
		}
		if err := writeScalarOrJSON(w, out); err != nil {
			return err
		}
	}
}

// writeJSONPath filters the result with a JSONPath template.
//
// The supported grammar is the part of the kubectl syntax that a command line
// actually uses: text outside braces is literal, and inside braces a path is
// written as $.a.b, .a.b, ['a']["b"], [0], [*] or [1:3]. Filters, recursive
// descent and functions are deliberately left out; use -o jq for those.
func writeJSONPath(w io.Writer, template string, v any) error {
	input, err := toGeneric(v)
	if err != nil {
		return err
	}
	var out strings.Builder
	rest := template
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:open])
		close := strings.IndexByte(rest[open:], '}')
		if close < 0 {
			return fmt.Errorf("invalid jsonpath %q: a { is not closed", template)
		}
		expr := rest[open+1 : open+close]
		values, err := evalPath(expr, input)
		if err != nil {
			return fmt.Errorf("invalid jsonpath %q: %w", template, err)
		}
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = scalarString(value)
		}
		out.WriteString(strings.Join(parts, " "))
		rest = rest[open+close+1:]
	}
	text := out.String()
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, err = io.WriteString(w, text)
	return err
}

// evalPath walks a JSONPath expression over a generic value.
func evalPath(expr string, root any) ([]any, error) {
	steps, err := parsePath(expr)
	if err != nil {
		return nil, err
	}
	current := []any{root}
	for _, step := range steps {
		var next []any
		for _, value := range current {
			out, err := step.apply(value)
			if err != nil {
				return nil, err
			}
			next = append(next, out...)
		}
		current = next
	}
	return current, nil
}

// pathStep is one selector of a JSONPath expression.
type pathStep struct {
	// field selects a mapping key.
	field string
	// index selects one element of a list.
	index *int
	// wildcard selects every element or value.
	wildcard bool
	// slice selects a range of a list; from and to may be nil.
	slice    bool
	from, to *int
}

func parsePath(expr string) ([]pathStep, error) {
	s := strings.TrimSpace(expr)
	s = strings.TrimPrefix(s, "$")
	var steps []pathStep
	for s != "" {
		switch {
		case strings.HasPrefix(s, "."):
			s = s[1:]
			if strings.HasPrefix(s, "*") {
				steps = append(steps, pathStep{wildcard: true})
				s = s[1:]
				continue
			}
			// A dot before a bracket, as in "{.[*].name}" over a top level
			// array, selects nothing of its own.
			if strings.HasPrefix(s, "[") {
				continue
			}
			end := strings.IndexAny(s, ".[")
			if end < 0 {
				end = len(s)
			}
			if end == 0 {
				return nil, fmt.Errorf("empty field name")
			}
			steps = append(steps, pathStep{field: s[:end]})
			s = s[end:]
		case strings.HasPrefix(s, "["):
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return nil, fmt.Errorf("a [ is not closed")
			}
			step, err := parseBracket(s[1:end])
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)
			s = s[end+1:]
		default:
			end := strings.IndexAny(s, ".[")
			if end < 0 {
				end = len(s)
			}
			steps = append(steps, pathStep{field: s[:end]})
			s = s[end:]
		}
	}
	return steps, nil
}

func parseBracket(inner string) (pathStep, error) {
	inner = strings.TrimSpace(inner)
	switch {
	case inner == "*":
		return pathStep{wildcard: true}, nil
	case strings.HasPrefix(inner, "'") && strings.HasSuffix(inner, "'"),
		strings.HasPrefix(inner, `"`) && strings.HasSuffix(inner, `"`):
		return pathStep{field: inner[1 : len(inner)-1]}, nil
	case strings.Contains(inner, ":"):
		lo, hi, _ := strings.Cut(inner, ":")
		step := pathStep{slice: true}
		if strings.TrimSpace(lo) != "" {
			n, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				return step, fmt.Errorf("invalid slice bound %q", lo)
			}
			step.from = &n
		}
		if strings.TrimSpace(hi) != "" {
			n, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return step, fmt.Errorf("invalid slice bound %q", hi)
			}
			step.to = &n
		}
		return step, nil
	default:
		n, err := strconv.Atoi(inner)
		if err != nil {
			return pathStep{}, fmt.Errorf("invalid index %q", inner)
		}
		return pathStep{index: &n}, nil
	}
}

func (s pathStep) apply(value any) ([]any, error) {
	switch {
	case s.field != "":
		m, ok := value.(map[string]any)
		if !ok {
			return nil, nil
		}
		v, ok := m[s.field]
		if !ok {
			return nil, nil
		}
		return []any{v}, nil
	case s.wildcard:
		switch v := value.(type) {
		case []any:
			return v, nil
		case map[string]any:
			out := make([]any, 0, len(v))
			for _, k := range sortedKeys(v) {
				out = append(out, v[k])
			}
			return out, nil
		default:
			return nil, nil
		}
	case s.index != nil:
		list, ok := value.([]any)
		if !ok {
			return nil, nil
		}
		i := *s.index
		if i < 0 {
			i += len(list)
		}
		if i < 0 || i >= len(list) {
			return nil, nil
		}
		return []any{list[i]}, nil
	case s.slice:
		list, ok := value.([]any)
		if !ok {
			return nil, nil
		}
		from, to := 0, len(list)
		if s.from != nil {
			from = clamp(*s.from, len(list))
		}
		if s.to != nil {
			to = clamp(*s.to, len(list))
		}
		if from > to {
			return nil, nil
		}
		return list[from:to], nil
	default:
		return nil, fmt.Errorf("empty selector")
	}
}

func clamp(i, length int) int {
	if i < 0 {
		i += length
	}
	return min(max(i, 0), length)
}

// toGeneric converts a typed value into the maps, lists and scalars the
// query engines work on, using the json struct tags.
func toGeneric(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// writeScalarOrJSON prints a string as itself and anything else as JSON, so
// that a query selecting one name does not print it in quotes.
func writeScalarOrJSON(w io.Writer, v any) error {
	if s, ok := v.(string); ok {
		_, err := fmt.Fprintln(w, s)
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(raw))
	return err
}

// scalarString renders one JSONPath result.
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(raw)
	}
}
