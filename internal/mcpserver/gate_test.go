// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"strings"
	"testing"
)

// select_nodes listed another spelling of a protected host as unknown but not
// as protected, and plan_change accepted it.
func TestProtectedHostIsRecognisedUnderEverySpelling(t *testing.T) {
	f := start(t, setup{})
	for _, name := range []string{"wlm01.hpc.example.org", "WLM01.", "10.0.1.1", "wlm01.mgmt.hpc.example.org", "wlm01.example.org"} {
		t.Run(name, func(t *testing.T) {
			var out struct {
				Unknown   string `json:"unknown"`
				Protected string `json:"protected"`
			}
			f.call(t, "select_nodes", map[string]any{"expression": "exe1," + name}, &out)
			if !strings.HasPrefix(out.Protected, "wlm01") {
				t.Errorf("protected = %q, want wlm01", out.Protected)
			}

			msg := f.refused(t, "plan_change", map[string]any{"action": "drain", "nodes": "exe1," + name, "reason": "x"})
			if !strings.Contains(msg, "protected host wlm01") {
				t.Errorf("plan_change message = %q, want wlm01 refused as protected", msg)
			}
		})
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v", sent)
	}
}

func TestPlanRefusesANodeTheInventoryDoesNotKnow(t *testing.T) {
	f := start(t, setup{})
	msg := f.refused(t, "plan_change", map[string]any{"action": "drain", "nodes": "exe1,ghost1", "reason": "x"})
	if !strings.Contains(msg, "ghost1") || !strings.Contains(msg, "inventory does not know") {
		t.Errorf("message = %q, want ghost1 refused as unknown", msg)
	}
}

// A forced run lets a protected host through, and the plan put to the user
// would have to name it. The server never forces: whatever it was started
// with, a plan touching wlm01 is refused and names it.
func TestPlanIsNeverForced(t *testing.T) {
	f := start(t, setup{force: true})
	for _, nodes := range []string{"exe1,wlm01", "exe1,ghost1"} {
		msg := f.refused(t, "plan_change", map[string]any{"action": "drain", "nodes": nodes, "reason": "x"})
		if !strings.Contains(msg, strings.TrimPrefix(nodes, "exe1,")) || !strings.Contains(msg, "--force") {
			t.Errorf("plan_change on %s: message = %q, want it refused and named", nodes, msg)
		}
	}
	if sent := f.cluster.sent(); len(sent) != 0 {
		t.Errorf("sent %v", sent)
	}
}
