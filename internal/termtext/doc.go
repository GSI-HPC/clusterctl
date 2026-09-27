// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package termtext makes untrusted text safe to write to a terminal, and
// measures how many columns text takes there.
//
// Text from elsewhere, such as the output of a program on a remote host or
// in a container, an error it returned, or a value a service reported, is
// not trusted: a carriage return or a cursor movement can overwrite a line
// printed before, an escape sequence can retitle the terminal or write the
// clipboard, and a bidirectional control can show text in another order
// than it has. EscapeText and EscapeCell show such characters as visible
// escapes instead. A program that escapes everything with this package, and
// nothing with anything else, has one place to fix when a character turns
// out to need escaping too.
//
// # Escape policy
//
// The policy is a deny-list: a rune is escaped when it can move the cursor,
// change the state of the terminal, or change the order or the lines text
// is shown in, and is written as it is otherwise. Escaped are:
//
//   - every C0 control (U+0000 to U+001F) and DEL (U+007F), except that
//     EscapeText keeps newline and tab, which cannot move the cursor back
//     over text already written;
//   - every C1 control (U+0080 to U+009F), which some terminals read as the
//     introducer of a control sequence;
//   - the bidirectional controls U+061C, U+200E, U+200F, U+202A to U+202E
//     and U+2066 to U+2069;
//   - the line and paragraph separators U+2028 and U+2029;
//   - every byte that is not part of valid UTF-8, such as a lone 0x9b.
//
// Other invisible characters, such as a zero-width space, a byte order mark
// or a tag character, are written as they are: they change neither the
// cursor nor the terminal, and an allow-list that escaped them would escape
// text in scripts that need joiners and marks too. A caller that compares
// names should not rely on this package to make look-alikes visible.
//
// # Widths
//
// RuneWidth and Width approximate the columns a terminal gives text, one
// rune at a time: two for East Asian wide and fullwidth characters, none for
// combining marks and format characters, one for the rest. They do not
// cluster graphemes, so a sequence joined with U+200D, a skin tone modifier,
// a variation selector that turns a character into an emoji, or a flag is
// counted as the sum of its runes, which may be more than a terminal shows.
// Truncate shortens text to a number of columns with the same widths.
package termtext
