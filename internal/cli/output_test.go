// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// TestBadQueryIsRejectedBeforeTheCommandRuns covers review finding 2.12: the
// expression was compiled only when the result was printed, after the
// command had run on the nodes.
func TestBadQueryIsRejectedBeforeTheCommandRuns(t *testing.T) {
	for _, spec := range []string{"jq=.[", "jsonpath={[']}", `jsonpath={range .[*]}{.target.name}{end}`} {
		rec := &transport.Recorder{}
		_, err := run(t, harnessOptions{recorder: rec}, "exec", "-n", "exe0001", "-o", spec, "--", "touch", "/tmp/flag")
		if err == nil {
			t.Errorf("-o %s was accepted", spec)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("-o %s: exit code = %d, want %d", spec, got, want)
		}
		if calls := rec.Commands(); len(calls) != 0 {
			t.Errorf("-o %s: the command was sent before the expression was checked: %q", spec, calls)
		}
	}
}

// TestMalformedJSONPathDoesNotPanic covers review finding 10.2 through the
// command that does not load the site.
func TestMalformedJSONPathDoesNotPanic(t *testing.T) {
	_, err := run(t, harnessOptions{}, "version", "-o", "jsonpath={[']}")
	if err == nil {
		t.Fatal("a malformed jsonpath was accepted")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}
