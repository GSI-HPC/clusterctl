// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset_test

import (
	"testing"

	"github.com/GSI-HPC/clusterctl/nodeset"
)

// FuzzParseFold checks the two properties every expression must satisfy:
// parsing never panics, and folding is idempotent, so the printed form of a
// set parses back into the same set, every host spelled as before.
//
// A fuzzing worker gives up on an input that runs for ten seconds, and a huge
// set proves nothing about folding that a small one does not. The limits are
// lowered and long inputs skipped, so that every input is cheap and the time
// goes into variety instead; TestFoldLargeSets covers the size.
func FuzzParseFold(f *testing.F) {
	nodeset.LowerLimits(f, 1<<12)

	seeds := []string{
		"exe[1-10]", "exe0001", "login", "exe[1-2]-ib[0-1]",
		"exe[1-5]!exe3", "exe[1-3]&exe[2-9]", "exe[1-3]^exe[3-5]",
		"exe[01-10/3]", "10.0.1.[1-4]", "exe[1-2].hpc.example.org",
		"", ",", "[", "]", "exe[]", "exe[9-1]", "@group", "exe[1-2]x",
		"exe[1-3]&", "exe[1-3]!,exe2", "exe[1-3]&&exe2", "-exe1",
		"exe[0001-0010],exe11", "exe1,exe01", "exe7 exe08 exe9", "exe[08-12]!exe09",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, expr string) {
		if len(expr) > 1<<10 {
			return
		}
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
		// Contains ignores padding, so only an exact comparison notices a
		// fold that renamed exe3 to exe03.
		if a, b := first.Expand(), second.Expand(); !equal(a, b) {
			t.Fatalf("folding %q to %q changed the hosts: %v then %v", expr, folded, a, b)
		}
		// Hostlist has to name the same hosts too.
		if hl := first.Hostlist(); !equal(nodeset.MustParse(hl).Expand(), first.Expand()) {
			t.Fatalf("Hostlist of %q is %q, which names other hosts", expr, hl)
		}
	})
}
