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

// FuzzParseWith holds ParseWith to go-nodeset: it names the hosts
// go-nodeset names, and refuses what go-nodeset refuses and, besides, only
// a group reference without a name. go-nodeset fuzzes its own parser; this
// is about the check in front of it. The resolver answers the empty name,
// as an attribute source does, so that only the check refuses it.
func FuzzParseWith(f *testing.F) {
	for _, s := range []string{
		"exe[1-10]", "node-1", "a-[1-2]", "exe[1-3]&", "@", "@:", "@rack:", "@rack:R1", "@rack:*",
		"@*", "exe1,@rack:", "@rack:&exe1", "[", "]", "@a]", "@[", "exe[1-]", "@nested",
	} {
		f.Add(s)
	}
	res := &nodeset.MapResolver{Default: "rack", Groups: map[string]map[string]string{
		"rack": {"": "exe[1-9]", "R1": "exe[1-2]", "R2": "exe[3-4]!exe3", "nested": "@R1,@rack:", "a]": "x1"},
		"":     {"": "y[1-2]", "b": "y3"},
	}}
	f.Fuzz(func(t *testing.T, expr string) {
		// A large range proves nothing a small one does not, and costs
		// memory go-nodeset's limits allow.
		if len(expr) > 256 || bigNumber.MatchString(expr) {
			return
		}
		got, err := nodeexpr.ParseWith(expr, res)
		want, wantErr := nodeset.ParseWith(expr, res)
		switch {
		case err == nil && wantErr != nil:
			t.Fatalf("ParseWith(%q) = %s, which go-nodeset refuses: %v", expr, got, wantErr)
		case err == nil && got.String() != want.String():
			t.Fatalf("ParseWith(%q) = %s, go-nodeset %s", expr, got, want)
		case err != nil && wantErr == nil && !strings.Contains(err.Error(), "empty group name in @"):
			t.Fatalf("ParseWith(%q) refused what go-nodeset reads: %v", expr, err)
		}
	})
}
