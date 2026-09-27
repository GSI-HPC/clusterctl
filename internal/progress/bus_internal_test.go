// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress

import (
	"context"
	"io"
	"math"
	"strings"
	"testing"
)

// Span ids are the Bus's random base plus a count; the one that would be
// zero, which means no span, is skipped.
func TestTheSpanIDThatWouldBeZeroIsSkipped(t *testing.T) {
	t.Parallel()
	b := NewBus(Options{})
	b.base = math.MaxUint64
	_, s := Start(WithBus(context.Background(), b), KindCall, "ssh")
	if s.id != 1 {
		t.Errorf("the span after the largest id is %d, want 1", s.id)
	}
	s.End(nil)
	b.Close()
}

// A line the encoder refuses stops the log, as a write that fails does.
func TestALineThatCannotBeEncodedStopsTheLog(t *testing.T) {
	t.Parallel()
	l := NewLog(io.Discard, LogOptions{})
	l.mu.Lock()
	l.write(math.NaN())
	l.mu.Unlock()
	if err := l.Close(); err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Errorf("Close = %v, want the encoder's error", err)
	}
}
