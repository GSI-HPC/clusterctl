// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

func TestSanitize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in string
		max      int
		want     string
	}{
		{"plain text is unchanged", "exe0001: up 3 days", 512, "exe0001: up 3 days"},
		{"a clipboard write is shown, not done", "\x1b]52;c;ZXZpbA==\x07evil", 512, `\x1b]52;c;ZXZpbA==\x07evil`},
		{"a title change is shown", "\x1b]0;owned\x1b\\", 512, `\x1b]0;owned\x1b\`},
		{"a screen clear is shown", "\x1b[2Jgone", 512, `\x1b[2Jgone`},
		{"an 8-bit control sequence is shown", "\u009b31mred", 512, `\u009b31mred`},
		{"a bidirectional override is shown", "file\u202etxt.exe", 512, `file\u202etxt.exe`},
		{"bytes that are not UTF-8 are shown", "bad \xff\xfe", 512, `bad \xff\xfe`},
		{"a tab and a newline stay on the line", "a\tb\nc", 512, `a\tb\nc`},
		{"a line ending in CR loses nothing", "abc\r", 512, "abc"},
		{"a line ending in CRLF loses nothing", "abc\r\n", 512, `abc\n`},
		{"lines split by CRLF lose nothing", "a\r\nb", 512, `a\nb`},
		{"an error page with CRLF keeps its first letter", "exe0001-bmc: 502: <html>\r\n<head>", 512, `exe0001-bmc: 502: <html>\n<head>`},
		{"a meter shows its last reading", "50%\r100%\r", 512, "100%"},
		{"a carriage return overwrites in place", "abcdef\r12", 512, "12cdef"},
		{"a carriage return goes back to its own line", "a\nbc\rX", 512, `a\nXc`},
		{"a carriage return counts runes, not bytes", "ééé\rx", 512, "xéé"},
		{"a byte that is not UTF-8 is one column", "\xff\xffa\rb", 512, `b\xffa`},
		{"an escape is text to a carriage return", "\x1b[31m\rok", 512, "ok31m"},
		{"an escape written over text is still shown", "ok\r\x1b[1A", 512, `\x1b[1A`},
		{"a cut keeps whole runes", "ééé", 5, "éé"},
		{"a cut keeps no half of an escape's rune", "a\u202e", 4, `a\u2`},
		{"a cut counts the escapes", "\x1b\x1b\x1b", 8, `\x1b\x1b`},
		{"zero sets no bound", strings.Repeat("x", 2000), 0, strings.Repeat("x", 2000)},
		{"a long line is cut", strings.Repeat("x", 1<<20), 512, strings.Repeat("x", 512)},
		{"a long line with a meter is cut", strings.Repeat("x", 1<<20) + "\r12", 512, "12" + strings.Repeat("x", 510)},
	}
	for _, tc := range tests {
		got := progress.Sanitize(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("%s: Sanitize(%.40q, %d) = %.80q, want %.80q", tc.name, tc.in, tc.max, got, tc.want)
		}
	}
}

// FuzzSanitize checks that no input, however hostile, leaves anything a
// terminal would act on in what Sanitize returns, and that the result keeps
// to its bound. Remote output reaches a live display only through it.
func FuzzSanitize(f *testing.F) {
	for _, s := range []string{
		"", "plain", "abc\r\n", "50%\r100%\r", "abcdef\r12", "a\nbc\rX",
		"\x1b]52;c;ZXZpbA==\x07", "\x1b[2J", "\x1bP1$r\x1b\\", "\u009b31m", "\u202e\u2066\u061c",
		"\xff\xfe\xc3", "é\ré", "\r\r\r", "\t\x00\x7f", "\u2028\u2029",
	} {
		f.Add(s, uint16(512))
		f.Add(s, uint16(3))
	}
	f.Fuzz(func(t *testing.T, in string, bound uint16) {
		if len(in) > 1<<16 {
			return
		}
		max := int(bound % 2048)
		got := progress.Sanitize(in, max)
		if !utf8.ValidString(got) {
			t.Fatalf("Sanitize(%q, %d) = %q, which is not UTF-8", in, max, got)
		}
		if output.EscapeCell(got) != got || strings.ContainsAny(got, "\r\n\t\x1b") {
			t.Fatalf("Sanitize(%q, %d) = %q, which still holds a control character", in, max, got)
		}
		if max > 0 && len(got) > max {
			t.Fatalf("Sanitize(%q, %d) is %d bytes long", in, max, len(got))
		}
		if max == 0 && len(got) > 6*len(in) {
			t.Fatalf("Sanitize(%q) grew to %d bytes", in, len(got))
		}
		if again := progress.Sanitize(got, max); again != got {
			t.Fatalf("Sanitize is not idempotent for %q: %q, then %q", in, got, again)
		}
		if !strings.Contains(in, "\r") && output.EscapeCell(in) == in && (max == 0 || len(in) <= max) && got != in {
			t.Fatalf("Sanitize(%q, %d) = %q changed text that needed nothing", in, max, got)
		}
	})
}
