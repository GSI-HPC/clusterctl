// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import (
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
