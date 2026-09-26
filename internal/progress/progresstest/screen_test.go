// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progresstest

import (
	"io"
	"testing"
)

// The screen shows the bytes the way a terminal does: rows written over,
// moved up to and erased, what is below the cursor cleared, and anything
// else visible as text.
func TestScreenShowsWhatATerminalWould(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		width  int
		writes []string
		want   string
	}{
		{"plain lines", 0, []string{"one\n", "two\n"}, "one\ntwo\n"},
		{"a row not ended", 0, []string{"Continue? [y/N] "}, "Continue? [y/N] \n"},
		{"a carriage return writes over", 0, []string{"50%\r100%\n"}, "100%\n"},
		{"a row erased", 0, []string{"done\n", "counter", "\r\x1b[2K", "next\n"}, "done\nnext\n"},
		{"a region of three rows erased", 0,
			[]string{"above\n", "a\nb\nc", "\r\x1b[2K\x1b[1A\x1b[2K\x1b[1A\x1b[2K", "below\n"}, "above\nbelow\n"},
		{"a region drawn again in place", 0,
			[]string{"a\nb", "\r\x1b[2K\x1b[1A\x1b[2K", "c\nd"}, "c\nd\n"},
		{"up by a count", 0, []string{"a\nb\nc", "\x1b[2A", "\rX"}, "X\nb\nc\n"},
		{"the rest of a row erased", 0, []string{"abcdef\r", "ab\x1b[K"}, "ab\n"},
		{"all below erased", 0, []string{"a\nb\nc", "\x1b[1A\r\x1b[J", "z"}, "a\nz\n"},
		{"up past the top", 0, []string{"a", "\x1b[5A\rb"}, "b\n"},
		{"a sequence it does not know", 0, []string{"\x1b[?25lhidden"}, "^[[?25lhidden\n"},
		{"a control character", 0, []string{"bell\a\n"}, "bell^G\n"},
		{"split across writes", 0, []string{"✓ a\r\x1b", "[2", "K\xe2\x9c", "\x93 b"}, "✓ b\n"},
		{"a row too long wraps", 4, []string{"abcdef", "\r\x1b[2K"}, "abcd\n"},
		{"a row as long as the width does not", 4, []string{"abcd", "\r\x1b[2K"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Screen{Width: tc.width}
			for _, w := range tc.writes {
				if n, err := io.WriteString(s, w); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if got := s.String(); got != tc.want {
				t.Errorf("screen:\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}
