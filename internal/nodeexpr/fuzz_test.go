// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeexpr_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-nodeset"

	"github.com/GSI-HPC/clusterctl/internal/nodeexpr"
)

// bigNumber matches a number of four digits or more.
var bigNumber = regexp.MustCompile(`[0-9]{4}`)

// FuzzParse holds Parse to go-nodeset: it names the hosts go-nodeset names,
// and refuses what go-nodeset refuses and, besides, only a range without its
// last bound. go-nodeset fuzzes its own parser; this is about the check in
// front of it.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"exe[1-]", "exe[1-,5]", "exe[1-/2]", "exe[5,1-]", "exe[1-10]", "node-1", "a-[1-2]",
		"exe[1-2]-x", "exe[1-2]-ib[0-1]", "exe[1-3]&", "@", "@rack:", "[", "]", "-exe[1-]", "exe0[1-]",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		// A large range proves nothing a small one does not, and costs
		// memory go-nodeset's limits allow.
		if len(expr) > 256 || bigNumber.MatchString(expr) {
			return
		}
		got, err := nodeexpr.Parse(expr)
		want, wantErr := nodeset.Parse(expr)
		switch {
		case err == nil && wantErr != nil:
			t.Fatalf("Parse(%q) = %s, which go-nodeset refuses: %v", expr, got, wantErr)
		case err == nil && got.String() != want.String():
			t.Fatalf("Parse(%q) = %s, go-nodeset %s", expr, got, want)
		case err != nil && wantErr == nil && !strings.HasSuffix(err.Error(), "has no last bound"):
			t.Fatalf("Parse(%q) refused what go-nodeset reads: %v", expr, err)
		}
	})
}
