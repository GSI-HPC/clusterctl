// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package slurm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/clusterctl/nodeset"
)

// JobState is what Slurm said about the nodes of a power action.
type JobState struct {
	// Busy are the nodes in a state that runs jobs.
	Busy *nodeset.NodeSet
	// Strange are the nodes in a state the check does not know, with the
	// states as Slurm wrote them in States.
	Strange *nodeset.NodeSet
	States  []string
	// Missing are the nodes Slurm did not report at all.
	Missing *nodeset.NodeSet
}

// Idle reports whether every node is known to run no job.
func (s JobState) Idle() bool {
	return s.Busy.IsEmpty() && s.Strange.IsEmpty() && s.Missing.IsEmpty()
}

// String names every node that is not known to be idle, and why.
func (s JobState) String() string {
	var parts []string
	if !s.Busy.IsEmpty() {
		parts = append(parts, fmt.Sprintf("%s %s running Slurm jobs", s.Busy, isAre(s.Busy)))
	}
	if !s.Strange.IsEmpty() {
		quoted := make([]string, len(s.States))
		for i, state := range s.States {
			// The state comes from the Slurm host; quoting it keeps control
			// characters off the terminal.
			quoted[i] = strconv.Quote(state)
		}
		parts = append(parts, fmt.Sprintf("Slurm reports %s in a state not known to be free of jobs (%s)",
			s.Strange, strings.Join(quoted, ", ")))
	}
	if !s.Missing.IsEmpty() {
		pronoun := "they run"
		if s.Missing.Len() == 1 {
			pronoun = "it runs"
		}
		parts = append(parts, fmt.Sprintf("Slurm did not report %s, so whether %s jobs is not known "+
			"(check that the Slurm and inventory names agree)", s.Missing, pronoun))
	}
	return strings.Join(parts, "; ")
}

// hostlistLimit is the longest host list handed to sinfo. A longer one
// would come close to the limit on one argument, so sinfo is asked about
// every node instead and the answer is narrowed here.
const hostlistLimit = 16 << 10

// JobCheck asks Slurm about the state of every node of a set that is to be
// powered off, and keeps the busiest answer for each.
func (c *Client) JobCheck(ctx context.Context, nodes *nodeset.NodeSet) (JobState, error) {
	// --all includes the nodes of hidden partitions, which run jobs too.
	argv := []string{"sinfo", "--all", "-h", "-N", "-o", "%N %T"}
	if hostlist := nodes.Hostlist(); len(hostlist) <= hostlistLimit {
		argv = append(argv, "-n", hostlist)
	}
	// An administrator is waiting to power the nodes off, so the check
	// waits for sinfo only so long.
	quick := *c
	quick.Timeout = 30 * time.Second
	result, err := quick.read(ctx, argv)
	if err != nil {
		return JobState{}, err
	}

	// sinfo lists a node once per partition; the busiest answer counts.
	verdict := map[string]jobVerdict{}
	stateOf := map[string]string{}
	for _, line := range result.Lines() {
		fields := strings.Fields(line)
		if len(fields) == 0 || !nodes.Contains(fields[0]) {
			continue
		}
		name, state := fields[0], ""
		if len(fields) > 1 {
			state = fields[1]
		}
		if v := classifyState(state); v > verdict[name] {
			verdict[name] = v
			stateOf[name] = state
		}
	}

	out := JobState{Busy: nodeset.New(), Strange: nodeset.New(), Missing: nodeset.New()}
	seen := map[string]bool{}
	for _, name := range nodes.Expand() {
		switch verdict[name] {
		case unreported:
			_ = out.Missing.Add(name)
		case strange:
			_ = out.Strange.Add(name)
			if state := stateOf[name]; !seen[state] {
				seen[state] = true
				out.States = append(out.States, state)
			}
		case busy:
			_ = out.Busy.Add(name)
		}
	}
	return out, nil
}

// jobVerdict is what a state says about jobs, ordered so that the more
// cautious verdict is the larger one.
type jobVerdict int

const (
	unreported jobVerdict = iota
	idle
	strange
	busy
)

// classifyState reads a %T state. A compound state such as
// "mixed+drain" is busy when any part is, and idle only when every part is
// known to run no job.
//
// Only the states Slurm prints for a node without jobs count as idle. Slurm
// prints "fail" for a mixed node with the fail flag, and "maint" and
// "reboot" for a node whose jobs are still completing, so those are not.
func classifyState(state string) jobVerdict {
	base := strings.TrimRight(strings.ToLower(state), stateFlags+"+")
	if base == "" {
		return strange
	}
	verdict := idle
	for part := range strings.SplitSeq(base, "+") {
		switch part {
		case "allocated", "alloc", "mixed", "mix", "completing", "comp",
			"draining", "drng", "failing", "failg":
			return busy
		case "idle", "drained", "drain", "down", "future", "futr",
			"planned", "plnd", "reserved", "resv",
			"power_down", "powering_down", "powered_down", "pow_dn":
		default:
			verdict = strange
		}
	}
	return verdict
}
