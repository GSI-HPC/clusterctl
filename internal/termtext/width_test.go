// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package termtext_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GSI-HPC/clusterctl/internal/termtext"
)

// A terminal gives CJK, Hangul, fullwidth forms and emoji two columns, a
// combining mark and a zero-width joiner none, and the rest one.
func TestTheWidthOfTextOnATerminal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want int
	}{
		{"exe0001", 7},
		{"失败", 4},
		{"✅ done", 7},
		{"한국어", 6},
		{"ＡＢ", 4},
		{"é", 1},
		{"a\u200db", 2},
		{"✓ ▸ … ·", 7},
		{"😀", 2},
	} {
		if got := termtext.Width(tc.text); got != tc.want {
			t.Errorf("Width(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

// Truncate never lets text reach past the columns given, and leaves out a
// wide character whole rather than split it.
func TestTruncate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		cols int
		want string
	}{
		{"exe0001", 7, "exe0001"},
		{"exe0001", 20, "exe0001"},
		{"exe0001", 3, "exe"},
		{"exe0001", 0, ""},
		{"exe0001", -1, ""},
		{"", 5, ""},
		{"失败了", 4, "失败"},
		{"失败了", 5, "失败"},
		{"a失败", 2, "a"},
		{"失败", 1, ""},
		{"e\u0301e\u0301", 1, "e\u0301"},
	} {
		got := termtext.Truncate(tc.text, tc.cols)
		if got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.text, tc.cols, got, tc.want)
		}
		if w := termtext.Width(got); w > max(tc.cols, 0) {
			t.Errorf("Truncate(%q, %d) is %d columns wide", tc.text, tc.cols, w)
		}
	}
}

// FuzzTruncate checks that Truncate returns a prefix of its input, cut on a
// rune boundary, that fits in the columns given, and the input itself when
// that fits already.
func FuzzTruncate(f *testing.F) {
	for _, seed := range []string{"", "exe0001", "失败了", "a\u200db", "😀 done"} {
		f.Add(seed, 4)
	}
	f.Fuzz(func(t *testing.T, in string, cols int) {
		if !utf8.ValidString(in) {
			t.Skip("Truncate takes text that is already escaped")
		}
		got := termtext.Truncate(in, cols)
		if !strings.HasPrefix(in, got) || !utf8.ValidString(got) {
			t.Fatalf("Truncate(%q, %d) = %q, not a prefix cut on a rune", in, cols, got)
		}
		if termtext.Width(got) > max(cols, 0) {
			t.Fatalf("Truncate(%q, %d) = %q, %d columns wide", in, cols, got, termtext.Width(got))
		}
		if cols > 0 && termtext.Width(in) <= cols && got != in {
			t.Fatalf("Truncate(%q, %d) = %q, though the text fits", in, cols, got)
		}
	})
}
