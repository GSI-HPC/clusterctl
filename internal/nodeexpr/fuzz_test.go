// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeexpr_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/GSI-HPC/go-nodeset"

	"github.com/GSI-HPC/clusterctl/internal/nodeexpr"
)

// bigNumber matches a number of four digits or more.
var bigNumber = regexp.MustCompile(`[0-9]{4}`)

// FuzzParseWith holds ParseWith to go-nodeset: looked up side by side
// through a Batch or one after the other, an expression names the same
// hosts, or fails with the same error. go-nodeset fuzzes its own parser;
// this is about the lookup in front of it, and about groupRefs, which reads
// an expression's references as go-nodeset does.
func FuzzParseWith(f *testing.F) {
	for _, s := range []string{
		"@a", "@a,@b", "@b!@a", "@e&@rack:r1", "@rack:*", "@*", "@a,@lost", "@lost,@a", "@gone,@nope",
		"@a@b", "@", "@site:", "n[1-3],@a ^ @rack:d", "@a,[", "@b]", "x[1-2]&@rack:r1,@c", "@:a,@s:g:h",
		"@rack:r2,@rack:r3", "@a\u00a0,@b", "exe[1-]",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		// A large range proves nothing a small one does not, and costs
		// memory go-nodeset's limits allow.
		if len(expr) > 256 || bigNumber.MatchString(expr) {
			return
		}
		got, err := nodeexpr.ParseWith(expr, newBatchResolver())
		want, wantErr := nodeset.ParseWith(expr, onlyResolver{newBatchResolver()})
		switch {
		case fmt.Sprint(err) != fmt.Sprint(wantErr):
			t.Fatalf("ParseWith(%q): error %v, go-nodeset %v", expr, err, wantErr)
		case err == nil && got.String() != want.String():
			t.Fatalf("ParseWith(%q) = %s, go-nodeset %s", expr, got, want)
		}
	})
}
