// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 3.4: slurmctld reads ALL as every node and a name it does not know stops
// an update half way, so a plan stands only for a set Slurm reads as itself.
func TestPlanRefusesNamesSlurmDoesNotReadAsThemselves(t *testing.T) {
	f := start(t, setup{})
	tests := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "resume", "nodes": "ALL"}, "every node"},
		{map[string]any{"action": "drain", "nodes": "exe1,all", "reason": "maint"}, "every node"},
		{map[string]any{"action": "drain", "nodes": "exe[1-2],zz1", "reason": "maint"}, "does not know zz1"},
	}
	for _, tc := range tests {
		// The safety gate refuses a name the inventory does not know before
		// Slurm is asked; either refusal keeps the plan from standing.
		if msg := f.refused(t, "plan_change", tc.args); !strings.Contains(msg, tc.want) &&
			!strings.Contains(msg, "which the inventory does not know") {
			t.Errorf("%v: message = %q, want %q", tc.args, msg, tc.want)
		}
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("planning sent %v", sent)
	}
	data, err := os.ReadFile(filepath.Join(f.stateDir, "mcp", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), `"outcome":"refused: `); got != len(tests) {
		t.Errorf("the audit has %d refusals, want %d:\n%s", got, len(tests), data)
	}
}

// 10.4, 12.1: the agent writes the reason, and it reaches the question put
// to the user and the node record in Slurm.
func TestPlanRefusesReasonsThatCouldMisleadTheUser(t *testing.T) {
	f := start(t, setup{})
	for _, tc := range []struct {
		action, reason, want string
	}{
		{"drain", "ticket 42\x1b[1A\x1b[2K\rAbout to drain 1 host: exe0001", "control character"},
		{"drain", "ticket 42\nContext staging, cluster test. This is a dry run; nothing is sent.", "control character"},
		{"drain", "ticket 42|exe0002|idle", "|"},
		{"drain", strings.Repeat("x", 201), "longer than"},
		{"drain", "none", "no reason"},
		{"resume", "ticket 42", "takes no reason"},
	} {
		args := map[string]any{"action": tc.action, "nodes": "exe[1-3]", "reason": tc.reason}
		if msg := f.refused(t, "plan_change", args); !strings.Contains(msg, tc.want) {
			t.Errorf("reason %q: message = %q, want %q", tc.reason, msg, tc.want)
		}
	}
}

// 10.4: the reason is quoted wherever the user reads it.
func TestPlanQuotesTheReason(t *testing.T) {
	f := start(t, setup{answer: accept(map[string]any{"confirm": true})})
	var detail struct {
		Detail string `json:"detail"`
	}
	f.call(t, "plan_change", map[string]any{"action": "drain", "nodes": "exe1", "reason": `ticket 4712: "fans"`}, &detail)
	if got, want := detail.Detail, `reason: "ticket 4712: \"fans\""`; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
	p := f.plan(t, map[string]any{"action": "drain", "nodes": "exe1", "reason": `ticket 4712: "fans"`})
	var out applyResult
	f.call(t, "apply_plan", applyArgs(p), &out)
	if len(f.asked) != 1 || !strings.Contains(f.asked[0], `reason: "ticket 4712: \"fans\""`) {
		t.Errorf("the user was asked %q", f.asked)
	}
}

// 12.6: sinfo prints "none" for a node without a reason; only real reasons
// are worth a warning.
func TestResumePlanWarnsOnlyOfRealReasons(t *testing.T) {
	f := start(t, setup{})
	p := f.plan(t, map[string]any{"action": "resume", "nodes": "exe[1-3]"})
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], `exe0001 was taken out with the reason "ticket 4711: DIMM"`) {
		t.Errorf("warnings = %q, want only the reason of exe0001", p.Warnings)
	}
}
