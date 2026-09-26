// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
)

// TestMain runs the tests with the progress settings of their own: the
// developer's TERM, CLUSTERCTL_PROGRESS and CLUSTERCTL_PROGRESS_LOG would
// choose other displays than the tests expect, or none, and the log would
// have every test append to the developer's own file; a CI job's
// TRACEPARENT would join their logs to its trace. A test that needs one of
// them sets it.
func TestMain(m *testing.M) {
	_ = os.Setenv("TERM", "xterm")
	for _, name := range []string{config.EnvProgress, config.EnvProgressLog, "TRACEPARENT", "TRACESTATE"} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
