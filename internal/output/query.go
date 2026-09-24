// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/itchyny/gojq"
)

// compileJQ parses and compiles a jq program.
//
// The program cannot read the environment: -o is chosen by whoever runs the
// command, which through the MCP server is an agent, and $ENV and env would
// hand it every variable of the process.
func compileJQ(program string) (code *gojq.Code, err error) {
	defer recoverQuery("jq", &err)
	query, err := gojq.Parse(program)
	if err != nil {
		return nil, fmt.Errorf("invalid jq expression %q: %w", program, err)
	}
	code, err = gojq.Compile(query, gojq.WithEnvironLoader(func() []string { return nil }))
	if err != nil {
		return nil, fmt.Errorf("invalid jq expression %q: %w", program, err)
	}
	return code, nil
}

// writeJQ filters the result with a jq program. The jq language is embedded,
// so no jq binary has to be installed. The program stops when ctx ends, since
// nothing else bounds one such as repeat(1).
func writeJQ(ctx context.Context, w io.Writer, program string, v any) (err error) {
	code, err := compileJQ(program)
	if err != nil {
		return err
	}
	input, err := toGeneric(v)
	if err != nil {
		return err
	}
	defer recoverQuery("jq", &err)
	iter := code.RunWithContext(ctx, input)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("jq: %w", err)
		}
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

// recoverQuery turns a panic in the query code into an error, so that a bad
// expression fails the one command rather than the process, which may be
// the MCP server.
func recoverQuery(what string, err *error) {
	if p := recover(); p != nil {
		*err = fmt.Errorf("%s: the expression could not be evaluated: %v", what, p)
	}
}

// templatePart is a piece of a JSONPath template: literal text, or a path
// whose values are printed in its place.
type templatePart struct {
	literal string
	path    []pathStep
	isPath  bool
}

// compileJSONPath parses a JSONPath template.
//
// The supported grammar is the part of the kubectl syntax that a command line
// actually uses: text outside braces is literal, and inside braces a path is
// written as $.a.b, .a.b, ['a']["b"], [0], [*] or [1:3]. Filters, recursive
// descent, functions, range and quoted literals are deliberately left out and
// rejected, rather than read as field names that match nothing; use -o jq
// for those.
func compileJSONPath(template string) (parts []templatePart, err error) {
	defer recoverQuery("jsonpath", &err)
	rest := template
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			if rest != "" {
				parts = append(parts, templatePart{literal: rest})
			}
			return parts, nil
		}
		if open > 0 {
			parts = append(parts, templatePart{literal: rest[:open]})
		}
		close := strings.IndexByte(rest[open:], '}')
		if close < 0 {
			return nil, fmt.Errorf("invalid jsonpath %q: a { is not closed", template)
		}
		steps, err := parsePath(rest[open+1 : open+close])
		if err != nil {
			return nil, fmt.Errorf("invalid jsonpath %q: %w", template, err)
		}
		parts = append(parts, templatePart{path: steps, isPath: true})
		rest = rest[open+close+1:]
	}
}

// writeJSONPath filters the result with a JSONPath template.
func writeJSONPath(w io.Writer, template string, v any) (err error) {
	parts, err := compileJSONPath(template)
	if err != nil {
		return err
	}
	input, err := toGeneric(v)
	if err != nil {
		return err
	}
	defer recoverQuery("jsonpath", &err)
	var out strings.Builder
	for _, part := range parts {
		if !part.isPath {
			out.WriteString(part.literal)
			continue
		}
		values, err := evalPath(part.path, input)
		if err != nil {
			return fmt.Errorf("invalid jsonpath %q: %w", template, err)
		}
		texts := make([]string, len(values))
		for i, value := range values {
			texts[i] = scalarString(value)
		}
		out.WriteString(strings.Join(texts, " "))
	}
	text := out.String()
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, err = io.WriteString(w, text)
	return err
}

// evalPath walks a parsed JSONPath expression over a generic value.
func evalPath(steps []pathStep, root any) ([]any, error) {
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
	if err := unsupported(s); err != nil {
		return nil, err
	}
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
			step, rest, err := parseField(s)
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)
			s = rest
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
			step, rest, err := parseField(s)
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)
			s = rest
		}
	}
	return steps, nil
}

// unsupported names the kubectl constructs this grammar leaves out and that
// would otherwise parse as field names, which match nothing and print an
// empty line, as if nothing had matched. Recursive descent, filters, unions
// and functions fail the grammar on their own.
func unsupported(expr string) error {
	word, _, _ := strings.Cut(expr, " ")
	switch {
	case word == "range" || word == "end":
		return fmt.Errorf("{%s} is not supported; use -o jq, as -o jq='.[] | .name'", expr)
	case strings.HasPrefix(expr, `"`) || strings.HasPrefix(expr, "'"):
		return fmt.Errorf("the quoted literal {%s} is not supported; write the text outside the braces, or use -o jq", expr)
	default:
		return nil
	}
}

// parseField reads a field name written after a dot, up to the next dot or
// bracket. A name that holds anything but letters, digits, _ and - has to be
// written in brackets and quotes, as ['@odata.id'].
func parseField(s string) (pathStep, string, error) {
	end := strings.IndexAny(s, ".[")
	if end < 0 {
		end = len(s)
	}
	name := s[:end]
	if name == "" {
		return pathStep{}, "", fmt.Errorf("empty field name; recursive descent is not supported, use -o jq")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return pathStep{}, "", fmt.Errorf("invalid field name %q; quote it in brackets, as ['%s']", name, name)
		}
	}
	return pathStep{field: name}, s[end:], nil
}

func parseBracket(inner string) (pathStep, error) {
	inner = strings.TrimSpace(inner)
	switch {
	case inner == "*":
		return pathStep{wildcard: true}, nil
	case strings.HasPrefix(inner, "'") || strings.HasPrefix(inner, `"`):
		// A lone quote is both the first and the last character, so the
		// length is checked before the quotes are stripped.
		if len(inner) < 2 || inner[len(inner)-1] != inner[0] {
			return pathStep{}, fmt.Errorf("a quote in %q is not closed", "["+inner+"]")
		}
		if len(inner) == 2 {
			return pathStep{}, fmt.Errorf("empty field name")
		}
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
			return pathStep{}, fmt.Errorf("invalid index %q; filters and unions are not supported, use -o jq", inner)
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
	// Numbers are kept as their JSON text: decoded into a float64, a PID
	// above 2^53 or an exit code of 0 would not print as it was.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
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
	case json.Number:
		return t.String()
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
