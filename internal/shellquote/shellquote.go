// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package shellquote renders an argument vector as a POSIX shell word list.
//
// Every command clusterctl sends to a remote host is quoted here and handed
// to ssh as a single argument. The remote shell then splits it back into
// exactly the arguments that were given locally. Building the remote command
// by joining arguments with spaces, which the shell toolkit did, let the
// local shell expand globs before sending, dropped repeated whitespace and
// broke on an apostrophe.
package shellquote

import "strings"

// safe reports whether a word needs no quoting at all. The set is
// deliberately small: anything outside it is quoted rather than reasoned
// about.
func safe(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == '/', c == ':', c == '=', c == '+', c == ',', c == '@', c == '%':
		default:
			return false
		}
	}
	return true
}

// Quote renders one word so that a POSIX shell reproduces it exactly.
func Quote(s string) string {
	if safe(s) {
		return s
	}
	// Single quotes protect everything except a single quote itself, which
	// is written by leaving the quoted run, escaping it, and starting a new
	// one.
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b.WriteString(`'\''`)
			continue
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('\'')
	return b.String()
}

// Join renders an argument vector as one shell command line.
func Join(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = Quote(a)
	}
	return strings.Join(quoted, " ")
}
