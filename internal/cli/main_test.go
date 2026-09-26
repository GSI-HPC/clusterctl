// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
)

// TestMain runs the tests with the progress settings of their own: the
// developer's TERM and CLUSTERCTL_PROGRESS would choose other displays than
// the tests expect, or none. A test that needs one of them sets it.
func TestMain(m *testing.M) {
	_ = os.Setenv("TERM", "xterm")
	for _, name := range []string{config.EnvProgress} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
