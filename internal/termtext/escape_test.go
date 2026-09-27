// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package termtext_test

import (
	"testing"

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
