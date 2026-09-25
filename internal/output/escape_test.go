// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output_test

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/output"
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
		if got := output.EscapeText(tc.in); got != tc.want {
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
		if got := output.EscapeCell(tc.in); got != tc.want {
			t.Errorf("EscapeCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTableCellCannotForgeARow covers the review's case: a value holding a
// newline rendered as a healthy row for another node.
func TestTableCellCannotForgeARow(t *testing.T) {
	t.Parallel()

	table := output.NewTable(output.Cols("NODE", "STATE", "REASON")...)
	table.Add("exe0001", "drained", "bad\nexe0002  idle     fine")
	table.Add("exe0003", "idle", hostile)
	table.Caption = "2 nodes\x1b[2J"

	for _, spec := range []string{"table", "wide"} {
		got := render(t, spec, output.Result{Table: table})
		lines := strings.SplitSeq(strings.TrimRight(got, "\n"), "\n")
		for line := range lines {
			if strings.HasPrefix(line, "exe0002") {
				t.Errorf("-o %s printed a forged row:\n%s", spec, got)
			}
		}
		if strings.ContainsAny(got, "\r\x1b\x07") {
			t.Errorf("-o %s wrote control characters: %q", spec, got)
		}
		if !strings.Contains(got, `bad\nexe0002`) {
			t.Errorf("-o %s does not show the escaped newline:\n%s", spec, got)
		}
	}
}

// TestTableAlignsEscapedCells checks that the width of a column is measured
// after escaping, so the columns still line up.
func TestTableAlignsEscapedCells(t *testing.T) {
	t.Parallel()

	table := output.NewTable(output.Cols("A", "B")...)
	table.Add("x\ty", "1")
	table.Add("long value", "2")
	got := render(t, "table", output.Result{Table: table})
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	col := strings.Index(lines[0], "B")
	for _, line := range lines[1:] {
		if len(line) <= col || line[col-1] != ' ' || line[col] == ' ' {
			t.Errorf("column B is not aligned:\n%s", got)
		}
	}
}
