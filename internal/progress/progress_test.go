// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress_test

import (
	"fmt"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// A value no constant declares still has a name, one that says what it is
// and which, so that a log or a test never shows an empty string for it.
func TestAValueWithoutAConstantHasAName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value fmt.Stringer
		want  string
	}{
		{progress.Kind(99), "kind(99)"},
		{progress.State(99), "state(99)"},
		{progress.Status(99), "status(99)"},
		{progress.Class(99), "class(99)"},
		{progress.Type(99), "type(99)"},
		{progress.Stream(99), "stream(99)"},
		{progress.ClassNone, "none"},
	} {
		if got := tc.value.String(); got != tc.want {
			t.Errorf("%T(%d).String() = %q, want %q", tc.value, tc.value, got, tc.want)
		}
	}
}

// Cache and Source set the fields of a lookup, which the events carry.
func TestALookupSaysWhereItsAnswerCameFrom(t *testing.T) {
	t.Parallel()

	ctx, bus, capture := watched(t, progress.Options{})
	_, span := progress.Start(ctx, progress.KindCall, "groups", progress.Cache("hit"), progress.Source("slurm"))
	span.End(nil)
	bus.Close()
	start := capture.Events()[0]
	if start.Cache != "hit" || start.Source != "slurm" {
		t.Errorf("the start carries cache %q and source %q, want hit and slurm", start.Cache, start.Source)
	}
}
