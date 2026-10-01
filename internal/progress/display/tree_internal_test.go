// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// longest picks out the targets that get a row where the frame used to sort
// them all; the rows are the same, in the same order.
func TestLongestPicksWhatSortingFound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		running := make([]*treeSpan, r.IntN(40))
		for i := range running {
			// Few distinct seconds, so that many targets tie.
			ran := now.Add(-time.Duration(r.IntN(5000)) * time.Millisecond)
			running[i] = &treeSpan{id: progress.SpanID(i), ran: ran}
		}
		sorted := slices.Clone(running)
		slices.SortStableFunc(sorted, func(a, b *treeSpan) int {
			return cmp.Compare(now.Sub(b.ran)/time.Second, now.Sub(a.ran)/time.Second)
		})
		for k := 0; k <= len(running)+1; k++ {
			want := sorted[:min(k, len(sorted))]
			if got := longest(slices.Clone(running), k, now); !slices.Equal(got, want) {
				t.Fatalf("longest of %d, k=%d: %v, want %v", len(running), k, ids(got), ids(want))
			}
		}
	}
}

func ids(spans []*treeSpan) []int {
	out := make([]int, len(spans))
	for i, s := range spans {
		out[i] = int(s.id)
	}
	return out
}
