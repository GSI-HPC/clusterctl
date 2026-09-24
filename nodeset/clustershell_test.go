// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset_test

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/nodeset"
)

// divergences are the places where clusterctl decides otherwise than
// ClusterShell on purpose. doc/nodeset.md lists each of them.
var divergences = map[string]bool{
	"padding identity":       true,
	"adjacent numeric parts": true,
	"padding mismatch":       true,
	"whitespace":             true,
	"empty operand":          true,
	"leading dash":           true,
}

// TestClusterShellCorpus runs every expression of testdata/clustershell.txt
// and compares the answer with the one ClusterShell gave. Where a line names a
// divergence the answers must differ, so that a divergence which goes away is
// noticed and taken out of the documentation too.
func TestClusterShellCorpus(t *testing.T) {
	t.Parallel()

	f, err := os.Open("testdata/clustershell.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	lines := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 2 || len(cols) > 3 {
			t.Fatalf("malformed corpus line %q", line)
		}
		expr, want, tag := cols[0], cols[1], ""
		if len(cols) == 3 {
			tag = cols[2]
			if !divergences[tag] {
				t.Fatalf("line %q names an undocumented divergence %q", line, tag)
			}
		}
		lines++

		got := answer(expr)
		switch {
		case tag == "" && got != sortWords(want):
			t.Errorf("%q: clusterctl gives %q, ClusterShell %q", expr, got, want)
		case tag != "" && got == sortWords(want):
			t.Errorf("%q: clusterctl now agrees with ClusterShell (%q); the %s divergence is gone", expr, got, tag)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines == 0 {
		t.Fatal("the corpus is empty")
	}
}

// answer expands an expression the way the corpus records it: the host names
// in sorted order, "-" for none, or "error".
func answer(expr string) string {
	ns, err := nodeset.Parse(expr)
	if err != nil {
		return "error"
	}
	if ns.IsEmpty() {
		return "-"
	}
	return sortWords(strings.Join(ns.Expand(), " "))
}

func sortWords(s string) string {
	words := strings.Fields(s)
	sort.Strings(words)
	return strings.Join(words, " ")
}
