// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package tmpl expands the {placeholder} templates the configuration uses for
// host names and tunnel definitions.
//
// The syntax is deliberately small: {name} is replaced by the value named
// "name", {{ and }} write a literal brace, and a placeholder that names
// nothing is an error rather than an empty string, so a typo in a naming rule
// is reported instead of producing a host name like ".example.org".
package tmpl

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Expand fills the placeholders of a template.
func Expand(template string, vars map[string]string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(template); {
		c := template[i]
		switch {
		case c == '{' && i+1 < len(template) && template[i+1] == '{':
			b.WriteByte('{')
			i += 2
		case c == '}' && i+1 < len(template) && template[i+1] == '}':
			b.WriteByte('}')
			i += 2
		case c == '{':
			end := strings.IndexByte(template[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("in %q: a { is not closed", template)
			}
			key := strings.TrimSpace(template[i+1 : i+end])
			value, ok := vars[key]
			if !ok {
				return "", fmt.Errorf("in %q: unknown placeholder {%s}; known are %s",
					template, key, strings.Join(Names(vars), ", "))
			}
			b.WriteString(value)
			i += end + 1
		case c == '}':
			return "", fmt.Errorf("in %q: a } has no opening brace", template)
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// Names lists the placeholders a variable set offers, in sorted order.
func Names(vars map[string]string) []string {
	return slices.Sorted(maps.Keys(vars))
}

// Prefixed copies a map of values under a dotted prefix, so that a domain
// table becomes {domains.hpc} and the like.
func Prefixed(prefix string, values map[string]string, into map[string]string) map[string]string {
	if into == nil {
		into = map[string]string{}
	}
	for k, v := range values {
		into[prefix+"."+k] = v
	}
	return into
}
