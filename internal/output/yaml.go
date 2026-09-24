// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// writeYAML renders a value as YAML.
//
// The value goes through JSON first, so that the json struct tags, which
// every type in this program carries, decide the field names, and numbers
// keep the text JSON gave them: an exit code stays 0 rather than 0.0.
//
// The emitter is written here rather than borrowed, because the output has
// to read back as exactly what was printed whichever YAML 1.1 or 1.2 reader
// a script uses: a string is left unquoted only when no resolver could take
// it for anything else, and it never carries a raw control character.
func writeYAML(w io.Writer, v any) error {
	generic, err := toGeneric(v)
	if err != nil {
		return err
	}
	var b strings.Builder
	yamlBlock(&b, generic, 0)
	_, err = io.WriteString(w, b.String())
	return err
}

// yamlBlock writes v on lines of its own at the given indent.
func yamlBlock(b *strings.Builder, v any, indent int) {
	pad := strings.Repeat(" ", indent)
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			break
		}
		for _, k := range sortedKeys(t) {
			b.WriteString(pad)
			b.WriteString(yamlString(k))
			b.WriteByte(':')
			yamlValue(b, t[k], indent, true)
		}
		return
	case []any:
		if len(t) == 0 {
			break
		}
		for _, item := range t {
			b.WriteString(pad)
			b.WriteByte('-')
			yamlValue(b, item, indent, false)
		}
		return
	}
	head, body := yamlScalar(v)
	b.WriteString(pad)
	b.WriteString(head)
	b.WriteByte('\n')
	yamlBody(b, body, indent+2)
}

// yamlValue writes the value that follows a "key:" or a "-" at the given
// indent. A sequence under a key is not indented further, which is how the
// goccy encoder printed it.
func yamlValue(b *strings.Builder, v any, indent int, underKey bool) {
	nested := false
	switch t := v.(type) {
	case map[string]any:
		nested = len(t) > 0
	case []any:
		nested = len(t) > 0
	}
	if !nested {
		head, body := yamlScalar(v)
		b.WriteByte(' ')
		b.WriteString(head)
		b.WriteByte('\n')
		yamlBody(b, body, indent+2)
		return
	}
	if underKey {
		b.WriteByte('\n')
		if _, isList := v.([]any); isList {
			yamlBlock(b, v, indent)
		} else {
			yamlBlock(b, v, indent+2)
		}
		return
	}
	// In a sequence the first line of a nested value follows the "- ".
	var sub strings.Builder
	yamlBlock(&sub, v, indent+2)
	b.WriteByte(' ')
	b.WriteString(sub.String()[indent+2:])
}

// yamlBody writes the lines of a block literal.
func yamlBody(b *strings.Builder, lines []string, indent int) {
	pad := strings.Repeat(" ", indent)
	for _, line := range lines {
		if line != "" {
			b.WriteString(pad)
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
}

// yamlScalar renders a value that is not a non-empty mapping or sequence. A
// string of several lines comes back as a block literal header and its
// lines.
func yamlScalar(v any) (string, []string) {
	switch t := v.(type) {
	case nil:
		return "null", nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		return yamlNumber(t), nil
	case string:
		if head, body, ok := yamlLiteral(t); ok {
			return head, body
		}
		return yamlString(t), nil
	case map[string]any:
		return "{}", nil
	case []any:
		return "[]", nil
	default:
		return yamlQuote(fmt.Sprint(t)), nil
	}
}

// yamlNumber renders a JSON number. JSON writes a large float as 1e+21,
// which a YAML 1.1 reader takes for a string; 1.0e+21 reads as a float in
// both versions.
func yamlNumber(n json.Number) string {
	s := n.String()
	if i := strings.IndexAny(s, "eE"); i >= 0 && !strings.Contains(s[:i], ".") {
		s = s[:i] + ".0" + s[i:]
	}
	return s
}

// yamlLiteral renders a string of several lines as a block literal, which
// keeps command output readable. It declines anything a literal cannot carry
// exactly: a character that is not printable, a carriage return among them,
// or a first line that is empty or starts with white space, which would
// throw off the reader's guess of the indentation.
func yamlLiteral(s string) (string, []string, bool) {
	content := strings.TrimRight(s, "\n")
	if !strings.Contains(s, "\n") || content == "" || content[0] == ' ' || content[0] == '\t' || content[0] == '\n' {
		return "", nil, false
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && !unicode.IsPrint(r) {
			return "", nil, false
		}
	}
	lines := strings.Split(content, "\n")
	switch trailing := len(s) - len(content); trailing {
	case 0:
		return "|-", lines, true
	case 1:
		return "|", lines, true
	default:
		for range trailing - 1 {
			lines = append(lines, "")
		}
		return "|+", lines, true
	}
}

// yamlString renders a string on one line, plain when that is safe and
// double quoted otherwise.
func yamlString(s string) string {
	if yamlPlain(s) {
		return s
	}
	return yamlQuote(s)
}

// yamlPlain reports whether a string can be written without quotes and read
// back as the same string by any YAML reader. It is deliberately strict: a
// string that has to be quoted only costs two characters.
func yamlPlain(s string) bool {
	if s == "" || strings.HasSuffix(s, " ") || strings.HasSuffix(s, ":") || strings.Contains(s, ": ") {
		return false
	}
	for i, r := range s {
		switch {
		case unicode.IsLetter(r), r == '_', r == '/':
		case i == 0:
			// A digit, a sign or a dot could start a number, a date or
			// .inf, and the other characters are indicators.
			return false
		case unicode.IsDigit(r), strings.ContainsRune(" .@+%~:,=()-", r):
		default:
			return false
		}
	}
	switch strings.ToLower(s) {
	case "y", "n", "yes", "no", "true", "false", "on", "off", "null":
		return false
	}
	return true
}

// yamlQuote renders a string in double quotes, escaping every character that
// is not printable.
func yamlQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
