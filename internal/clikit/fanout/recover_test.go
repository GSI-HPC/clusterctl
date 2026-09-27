// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout_test

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/clikit/fanout"
)

// Without a log, the stack of a panic goes to the process's standard
// error. The test swaps os.Stderr, so it does not run in parallel.
func TestRecoveredWritesToStandardErrorWithoutALog(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	perr := fanout.Recovered(nil, "sind", "worker-1", "boom")
	os.Stderr = saved
	_ = w.Close()
	written, _ := io.ReadAll(r)
	if !strings.HasPrefix(string(written), `sind: panic while working on worker-1: "boom"`) {
		t.Errorf("standard error reads %q", written)
	}
	if want := `sind panicked; this is a bug, please report it: "boom"`; perr == nil || perr.Error() != want {
		t.Errorf("Recovered = %v, want %q", perr, want)
	}
	if fanout.Recovered(nil, "sind", "worker-1", nil) != nil {
		t.Error("Recovered returned an error when nothing panicked")
	}
}
