// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package ipmi

import (
	"strings"
	"testing"
)

// lastLine used to split the whole error stream into lines to take the last
// one, for every processor the backend did not answer for. It finds the
// last line without splitting, and finds the same one.
func TestLastLineFindsWhatSplittingFound(t *testing.T) {
	t.Parallel()
	split := func(s string) string {
		lines := strings.Split(strings.TrimSpace(s), "\n")
		return strings.TrimSpace(lines[len(lines)-1])
	}
	for _, s := range []string{
		"", "\n", "  \n\t\n", "one", "one\n", "one\ntwo", "one\ntwo\n\n",
		"one\n  two  \n", "one\r\ntwo\r\n", "\n\none", "a\n\n\n", "x\n \n",
	} {
		if got, want := lastLine(s), split(s); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", s, got, want)
		}
	}
}
