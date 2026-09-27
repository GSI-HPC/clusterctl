// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout_test

import (
	"context"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/clikit/fanout"
)

// Once the context has ended, Each starts nothing, even when a free place
// and the end are ready at once and select takes the place: it asks the
// context again. Each round is a toss of select's, so the test tosses
// often enough that both sides come up.
func TestEachStartsNothingOnceTheContextHasEnded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		fanout.Each(ctx, 1, 1, func(int) { t.Fatal("work started on an ended context") })
	}
}
