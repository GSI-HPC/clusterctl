// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package termtext

import (
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

// RuneWidth returns how many columns a terminal gives r: two for a
// character whose East Asian Width is wide or fullwidth, as CJK, kana,
// Hangul and the emoji drawn as pictures are; none for a mark that
// combines with the character before it or a character that only formats,
// such as a zero-width joiner; and one for everything else.
func RuneWidth(r rune) int {
	if r == 0 || unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// Width returns how many columns s takes on a terminal, the widths of its
// runes added up.
func Width(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// Truncate shortens s to at most cols columns, as Width counts them, so that
// a row never wraps. A wide character that would reach past cols is left out
// whole, never split; cols of zero or less leaves nothing.
//
// s must be plain text that is already escaped, by EscapeCell for one line:
// a control character or an escape sequence would be counted as the columns
// of its runes, not as what it does. Truncate does not cluster graphemes,
// so it may cut a sequence of runes a terminal draws as one character, such
// as an emoji joined with U+200D, between its runes.
func Truncate(s string, cols int) string {
	if cols < 1 {
		return ""
	}
	if Width(s) <= cols {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		w := RuneWidth(r)
		if w > cols {
			break
		}
		b.WriteRune(r)
		cols -= w
	}
	return b.String()
}
