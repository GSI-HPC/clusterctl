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

// Split splits a shell command line into words, understanding single quotes,
// double quotes and backslash escapes. It is used where a user writes one
// command as a string, such as a group source definition in a file that was
// converted from an older format.
func Split(s string) ([]string, error) {
	var (
		words []string
		word  strings.Builder
		open  bool
	)
	flush := func() {
		if open {
			words = append(words, word.String())
			word.Reset()
			open = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'':
			open = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errUnterminated('\'')
			}
			word.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case '"':
			open = true
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				word.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, errUnterminated('"')
			}
			i = j
		case '\\':
			open = true
			if i+1 < len(s) {
				i++
				word.WriteByte(s[i])
			}
		default:
			open = true
			word.WriteByte(c)
		}
	}
	flush()
	return words, nil
}

type errUnterminated byte

func (e errUnterminated) Error() string {
	return "unterminated " + string(rune(e)) + " quote"
}
