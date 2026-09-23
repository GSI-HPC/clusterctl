// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset

import "testing"

// LowerLimits caps a range at n elements and an expression at n hosts until
// the test ends. It changes package state, so a test calling it must not run
// in parallel with other tests of the package.
func LowerLimits(tb testing.TB, n int) {
	tb.Helper()
	ranges, sets := maxRangeElements, maxSetElements
	maxRangeElements, maxSetElements = n, n
	tb.Cleanup(func() { maxRangeElements, maxSetElements = ranges, sets })
}
