// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// TestResultJSON covers review finding 12.3: the target was serialised with
// Go field names, so the documented .target.name was null, and the error
// text was left out of every machine format.
func TestResultJSON(t *testing.T) {
	t.Parallel()

	results := []*transport.Result{
		{Target: transport.Target{Name: "exe0001", Host: "exe0001.example.org", User: "root"}, Stdout: "ok\n"},
		{
			Target:   transport.Target{Name: "wlm01", Host: "wlm01.example.org", User: "admin", Role: "wlm", ForwardAgent: true, ForwardX11: true},
			ExitCode: 255,
			Err:      errors.New("wlm01: connection refused"),
		},
	}
	raw, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	want := []map[string]any{
		{
			"target":   map[string]any{"name": "exe0001", "host": "exe0001.example.org", "user": "root"},
			"exitCode": 0.0,
			"stdout":   "ok\n",
		},
		{
			"target": map[string]any{
				"name": "wlm01", "host": "wlm01.example.org", "user": "admin",
				"role": "wlm", "forwardAgent": true, "forwardX11": true,
			},
			"exitCode": 255.0,
			"error":    "wlm01: connection refused",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d:\n%s", len(got), len(want), raw)
	}
	for i := range want {
		g, _ := json.Marshal(got[i])
		w, _ := json.Marshal(want[i])
		if string(g) != string(w) {
			t.Errorf("result %d =\n%s\nwant\n%s", i, g, w)
		}
	}
}
