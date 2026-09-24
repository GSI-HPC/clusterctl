// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// EscapeText makes untrusted text safe to write to a terminal, keeping its
// lines.
//
// Output from a node, a BMC, Slurm or an agent is not trusted: a carriage
// return or a cursor movement can overwrite the line printed for another
// node, and an escape sequence can retitle the terminal or write the
// clipboard. EscapeText replaces every C0 and C1 control character, DEL,
// the Unicode controls that reorder or break lines, and bytes that are not
// UTF-8 with a visible escape such as \x1b or \u009b. Newline and tab are
// kept, since they cannot move the cursor back over text already printed.
//
// Text that holds none of these is returned unchanged.
func EscapeText(s string) string {
	return escape(s, false)
}

// EscapeCell is EscapeText for a value that has to stay on one line, such as
// a table cell or a name in a header: newline and tab are escaped too, as \n
// and \t, so that a value cannot start a row that seems to belong to another
// node.
func EscapeCell(s string) string {
	return escape(s, true)
}

func escape(s string, oneLine bool) string {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if needsEscape(r, size, oneLine) {
			break
		}
		i += size
	}
	if i == len(s) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + 16)
	b.WriteString(s[:i])
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case !needsEscape(r, size, oneLine):
			b.WriteString(s[i : i+size])
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x80:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		i += size
	}
	return b.String()
}

// needsEscape reports whether a rune may not reach a terminal as it is.
func needsEscape(r rune, size int, oneLine bool) bool {
	switch {
	case r == utf8.RuneError && size == 1:
		// A byte that is not UTF-8, such as a lone 0x9b, which some
		// terminals read as the start of a control sequence.
		return true
	case r == '\n' || r == '\t':
		return oneLine
	case r < 0x20, r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x2066 && r <= 0x2069:
		// Bidirectional controls, which make a terminal show text in an
		// order other than the one it has.
		return true
	case r == 0x2028, r == 0x2029:
		// The Unicode line and paragraph separators.
		return true
	default:
		return false
	}
}
