// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/GSI-HPC/go-nodeset"

	"github.com/GSI-HPC/clusterctl/internal/slurm"
)

// TestNodesWithJobs checks which nodes of a set a queue keeps busy: those a
// job runs on, whatever else the job runs on, and none for a job whose
// nodes cannot be read.
func TestNodesWithJobs(t *testing.T) {
	jobs := []slurm.Job{
		{ID: "1", Nodes: "exe[1-3]"},
		{ID: "2", Nodes: "exe[3-4],sub1"},
		{ID: "3", Nodes: "exe["},
		{ID: "4", Nodes: "exe9"},
	}
	got := nodesWithJobs(jobs, nodeset.MustParse("exe[2-5]"))
	if got.String() != "exe[2-4]" {
		t.Errorf("busy nodes are %s, want exe[2-4]", got)
	}
}

// TestNodesWithJobsIsLinear checks that a long queue costs in proportion to
// its length. The nodes of each job were added to a copy of the set built so
// far: 1.2 s and 1.9 GB for a plan over 5,000 busy nodes. It measures
// allocation across the whole process, so it must not run in parallel.
func TestNodesWithJobsIsLinear(t *testing.T) {
	allocated := func(n int) uint64 {
		jobs := make([]slurm.Job, n)
		for i := range jobs {
			jobs[i] = slurm.Job{ID: fmt.Sprint(i), Nodes: fmt.Sprintf("exe%05d", i+1)}
		}
		ns := nodeset.MustParse(fmt.Sprintf("exe[00001-%05d]", n))
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := nodesWithJobs(jobs, ns)
		runtime.ReadMemStats(&after)
		if got.Len() != n {
			t.Fatalf("%d busy nodes, want %d", got.Len(), n)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := allocated(500), allocated(5000)
	if large > 20*small {
		t.Errorf("a queue of 5,000 jobs allocated %d KiB, %d times what 500 cost; it should be about 10",
			large>>10, large/small)
	}
}
