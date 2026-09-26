// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output_test

import (
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/output"
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
		if got := output.Width(tc.text); got != tc.want {
			t.Errorf("Width(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}
