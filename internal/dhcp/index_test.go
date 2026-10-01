// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package dhcp_test

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/dhcp"
)

// scan finds what Lookup and Mentions find by reading every declaration, the
// way they did before they had an index.
func scan(c *dhcp.Config, node string) (named, mentioned []string) {
	words := func(comment string) []string {
		return strings.FieldsFunc(comment, func(r rune) bool { return strings.ContainsRune(" \t,:;()", r) })
	}
	for _, h := range c.Hosts {
		byName := node != "" && (h.Name == node || strings.HasPrefix(h.Name, node+".") ||
			strings.HasPrefix(h.Name, node+"-") || strings.HasPrefix(h.Name, node+"_"))
		if byName {
			named = append(named, h.Name)
			continue
		}
		for _, comment := range h.Comments {
			if node != "" && slices.Contains(words(comment), node) {
				mentioned = append(mentioned, h.Name)
				break
			}
		}
	}
	return named, mentioned
}

func names(matches []dhcp.Match) []string {
	var out []string
	for _, m := range matches {
		out = append(out, m.Name)
	}
	return out
}

// The index finds exactly what reading every declaration finds, in the same
// order, for names that are prefixes of each other, other interfaces, fully
// qualified names, names a comment gives, and declarations given twice.
func TestTheIndexFindsWhatAScanFinds(t *testing.T) {
	t.Parallel()

	conf := `
host exe1 { fixed-address 10.0.0.1; }
host exe10 { fixed-address 10.0.0.10; }
host exe1-bmc { fixed-address 10.1.0.1; }
host exe1_ib { fixed-address 10.2.0.1; }
host exe1.hpc.example.org { fixed-address 10.0.0.1; }
host exe1x { fixed-address 10.0.0.99; }
# rack R2 (exe1, exe2): spare
host spare { fixed-address 10.0.0.200; }
# exe2
host exe2-bmc { fixed-address 10.1.0.2; }
host exe1 { fixed-address 10.0.0.2; }
host exe { fixed-address 10.0.0.250; }
host exe1- { fixed-address 10.0.0.251; }
`
	parsed, err := dhcp.Parse([]byte(conf))
	if err != nil {
		t.Fatal(err)
	}
	// The same declarations in the order of the file rather than of
	// their names, as a configuration built by hand may be.
	unsorted := &dhcp.Config{Hosts: slices.Clone(parsed.Hosts)}
	slices.Reverse(unsorted.Hosts)

	for _, c := range []*dhcp.Config{parsed, unsorted} {
		for _, node := range []string{"exe1", "exe10", "exe2", "exe", "spare", "rack", "R2", "exe1-bmc", "x", ""} {
			named, mentioned := scan(c, node)
			if got := names(c.Lookup(node)); !slices.Equal(got, named) {
				t.Errorf("Lookup(%q) = %v, want %v", node, got, named)
			}
			if got := names(c.Mentions(node)); !slices.Equal(got, mentioned) {
				t.Errorf("Mentions(%q) = %v, want %v", node, got, mentioned)
			}
		}
	}
}

// Every lookup read every declaration, so that dhcp hosts over 10,000 nodes
// took half a minute. Looking up ten times as many nodes in ten times as
// many declarations has to cost about ten times as much, not a hundred.
func TestLookupsDoNotReadEveryDeclaration(t *testing.T) {
	cost := func(n int) time.Duration {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "# chassis C%d: exe%05d\nhost exe%05d { fixed-address 10.%d.%d.%d; }\nhost exe%05d-bmc { fixed-address 10.200.%d.%d; }\n",
				i/16, i, i, i>>16, (i>>8)&255, i&255, i, (i>>8)&255, i&255)
		}
		c, err := dhcp.Parse([]byte(b.String()))
		if err != nil {
			t.Fatal(err)
		}
		best := time.Duration(1 << 62)
		for range 5 {
			runtime.GC()
			start := time.Now()
			for i := range n {
				node := fmt.Sprintf("exe%05d", i)
				if len(c.Lookup(node)) != 2 || len(c.Mentions(node)) != 0 {
					t.Fatalf("the declarations of %s were not found", node)
				}
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	// Large enough sets that the time is the work's and not the timer's or
	// the scheduler's: 800 nodes took under a millisecond, and a macOS
	// runner made 8,000 take 37 times that.
	small, large := cost(2000), cost(20000)
	if large > 30*small {
		t.Errorf("looking up 20,000 nodes took %v, %d times what 2,000 took; it should be about 10",
			large, large/max(small, 1))
	}
}
