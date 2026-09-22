// SPDX-License-Identifier: LGPL-3.0-or-later

package tmpl_test

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/tmpl"
)

func TestExpand(t *testing.T) {
	t.Parallel()

	vars := tmpl.Prefixed("domains", map[string]string{"hpc": "hpc.example.org"},
		map[string]string{"name": "exe1", "bmcPrefix": "bmc-"})

	tests := []struct{ in, want string }{
		{"{name}", "exe1"},
		{"{name}.{domains.hpc}", "exe1.hpc.example.org"},
		{"{bmcPrefix}{name}", "bmc-exe1"},
		{"plain", "plain"},
		{"{{literal}}", "{literal}"},
		{"", ""},
	}
	for _, tc := range tests {
		got, err := tmpl.Expand(tc.in, vars)
		if err != nil {
			t.Errorf("Expand(%q) failed: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExpandReportsMistakes(t *testing.T) {
	t.Parallel()

	vars := map[string]string{"name": "exe1"}
	for _, in := range []string{"{name", "{nope}", "a } b"} {
		if _, err := tmpl.Expand(in, vars); err == nil {
			t.Errorf("Expand(%q) should fail", in)
		}
	}

	// The message lists what could have been meant.
	_, err := tmpl.Expand("{nope}", vars)
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("error = %v, want it to list the known placeholders", err)
	}
}

func TestNamesAreSorted(t *testing.T) {
	t.Parallel()

	got := tmpl.Names(map[string]string{"b": "", "a": "", "c": ""})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("Names = %v, want them sorted", got)
	}
}
