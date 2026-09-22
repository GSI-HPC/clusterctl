// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset_test

import (
	"testing"

	"github.com/GSI-HPC/clusterctl/nodeset"
)

// FuzzParseFold checks the two properties every expression must satisfy:
// parsing never panics, and folding is idempotent, so the printed form of a
// set parses back into the same set.
func FuzzParseFold(f *testing.F) {
	seeds := []string{
		"exe[1-10]", "exe0001", "login", "exe[1-2]-ib[0-1]",
		"exe[1-5]!exe3", "exe[1-3]&exe[2-9]", "exe[1-3]^exe[3-5]",
		"exe[01-10/3]", "10.0.1.[1-4]", "exe[1-2].hpc.example.org",
		"", ",", "[", "]", "exe[]", "exe[9-1]", "@group", "exe[1-2]x",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, expr string) {
		first, err := nodeset.Parse(expr)
		if err != nil {
			return
		}
		folded := first.String()
		second, err := nodeset.Parse(folded)
		if err != nil {
			t.Fatalf("Parse(%q) accepted, but its folded form %q did not parse: %v", expr, folded, err)
		}
		if got := second.String(); got != folded {
			t.Fatalf("folding is not idempotent for %q: %q then %q", expr, folded, got)
		}
		if first.Len() != second.Len() {
			t.Fatalf("folding %q changed the host count: %d then %d", expr, first.Len(), second.Len())
		}
		for _, name := range first.Expand() {
			if !second.Contains(name) {
				t.Fatalf("folding %q to %q lost host %q", expr, folded, name)
			}
		}
	})
}
