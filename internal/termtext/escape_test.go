// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package termtext_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GSI-HPC/clusterctl/internal/termtext"
)

// hostile is what a compromised node answered in the review: a carriage
// return and cursor-up to overwrite the line printed for another node, and
// OSC 52 to write the clipboard.
const hostile = "ok\r\x1b[1Aexe0001: \x1b]52;c;ZXZpbA==\x07evil"

func TestEscapeText(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"two\nlines\n", "two\nlines\n"},
		{"tab\tseparated", "tab\tseparated"},
		{hostile, `ok\r\x1b[1Aexe0001: \x1b]52;c;ZXZpbA==\x07evil`},
		{"nul\x00del\x7f", `nul\x00del\x7f`},
		{"c1 \u009b31m", `c1 \u009b31m`},
		{"bidi \u202eevil", `bidi \u202eevil`},
		{"lines\u2028and\u2029paragraphs", `lines\u2028and\u2029paragraphs`},
		{"zero\u200bwidth", "zero\u200bwidth"},
		{"bad utf-8 \xff\xfe", `bad utf-8 \xff\xfe`},
		{"grüße", "grüße"},
	}
	for _, tc := range tests {
		if got := termtext.EscapeText(tc.in); got != tc.want {
			t.Errorf("EscapeText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapeCell(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"firmware update, ticket 4711", "firmware update, ticket 4711"},
		{"bad\nexe0002  idle   fine", `bad\nexe0002  idle   fine`},
		{"cr\rtab\t", `cr\rtab\t`},
		{hostile, `ok\r\x1b[1Aexe0001: \x1b]52;c;ZXZpbA==\x07evil`},
	}
	for _, tc := range tests {
		if got := termtext.EscapeCell(tc.in); got != tc.want {
			t.Errorf("EscapeCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// FuzzEscape checks what the escapers promise of any input: the result is
// valid UTF-8 and holds no rune the policy escapes, EscapeCell's result is
// one line, and text that needs no escape comes back unchanged.
func FuzzEscape(f *testing.F) {
	for _, seed := range []string{"", "plain", hostile, "a\nb\tc", "\xff\xfe", "\u009b", "\u2028", "\u202e", "失败"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		text, cell := termtext.EscapeText(in), termtext.EscapeCell(in)
		for name, got := range map[string]string{"EscapeText": text, "EscapeCell": cell} {
			if !utf8.ValidString(got) {
				t.Fatalf("%s(%q) = %q, not UTF-8", name, in, got)
			}
			for _, r := range got {
				if r < 0x20 && r != '\n' && r != '\t' || r >= 0x7f && r <= 0x9f ||
					r == 0x061c || r == 0x200e || r == 0x200f || r >= 0x202a && r <= 0x202e ||
					r >= 0x2066 && r <= 0x2069 || r == 0x2028 || r == 0x2029 {
					t.Fatalf("%s(%q) = %q, which holds %U", name, in, got, r)
				}
			}
		}
		if strings.ContainsAny(cell, "\n\t") {
			t.Fatalf("EscapeCell(%q) = %q, not one line", in, cell)
		}
		if termtext.EscapeCell(cell) != cell {
			t.Fatalf("EscapeCell(%q) = %q, which EscapeCell changes again", in, cell)
		}
		if termtext.EscapeText(text) != text {
			t.Fatalf("EscapeText(%q) = %q, which EscapeText changes again", in, text)
		}
		if cell == in && text != in {
			t.Fatalf("EscapeText(%q) = %q, though EscapeCell changes nothing", in, text)
		}
	})
}
