// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
)

// A traceparent is taken only as W3C writes it, and anything else is no
// trace context at all; the tracestate is kept as it is when it can be.
func TestParseTraceContext(t *testing.T) {
	t.Parallel()

	const (
		trace  = "4bf92f3577b34da6a3ce929d0e0e4736"
		parent = "00f067aa0ba902b7"
		valid  = "00-" + trace + "-" + parent + "-01"
		state  = "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE"
	)
	tests := []struct {
		name         string
		header, st   string
		ok           bool
		flags        byte
		wantState    string
		wantTrace    string
		wantParentID string
	}{
		{"sampled", valid, "", true, 0x01, "", trace, parent},
		{"not sampled", "00-" + trace + "-" + parent + "-00", "", true, 0x00, "", trace, parent},
		{"flags not yet defined are kept", "00-" + trace + "-" + parent + "-ff", "", true, 0xff, "", trace, parent},
		{"with a tracestate", valid, state, true, 0x01, state, trace, parent},
		{"a later version, with more after it", "01-" + trace + "-" + parent + "-01-what-comes-next", "", true, 0x01, "", trace, parent},
		{"a later version, just as long", "cc-" + trace + "-" + parent + "-01", "", true, 0x01, "", trace, parent},

		{"empty", "", "", false, 0, "", "", ""},
		{"version ff", "ff-" + trace + "-" + parent + "-01", "", false, 0, "", "", ""},
		{"version 00 with more after it", valid + "-00", "", false, 0, "", "", ""},
		{"a later version with no dash after the flags", "01-" + trace + "-" + parent + "-01x", "", false, 0, "", "", ""},
		{"a trace id of zeros", "00-" + strings.Repeat("0", 32) + "-" + parent + "-01", "", false, 0, "", "", ""},
		{"a span id of zeros", "00-" + trace + "-" + strings.Repeat("0", 16) + "-01", "", false, 0, "", "", ""},
		{"upper case", "00-" + strings.ToUpper(trace) + "-" + parent + "-01", "", false, 0, "", "", ""},
		{"upper case flags", "00-" + trace + "-" + parent + "-0A", "", false, 0, "", "", ""},
		{"a digit that is not hexadecimal", "0g-" + trace + "-" + parent + "-01", "", false, 0, "", "", ""},
		{"too short", "00-" + trace[1:] + "-" + parent + "-01", "", false, 0, "", "", ""},
		{"another separator", "00_" + trace + "_" + parent + "_01", "", false, 0, "", "", ""},
		{"a space in front", " " + valid, "", false, 0, "", "", ""},
		{"a newline after it", valid + "\n", "", false, 0, "", "", ""},
		{"a tracestate without a valid traceparent", "00-" + trace + "-" + parent, state, false, 0, "", "", ""},

		{"a tracestate with a control character is left out", valid, "rojo=1\x1b[2J", true, 0x01, "", trace, parent},
		{"a tracestate that is not ASCII is left out", valid, "rojo=é", true, 0x01, "", trace, parent},
		{"a tracestate too long is left out", valid, "rojo=" + strings.Repeat("a", 508), true, 0x01, "", trace, parent},
		{"a tracestate of 512 bytes is kept", valid, "rojo=" + strings.Repeat("a", 507), true, 0x01, "rojo=" + strings.Repeat("a", 507), trace, parent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := progress.ParseTraceContext(tc.header, tc.st)
			if ok != tc.ok {
				t.Fatalf("ParseTraceContext(%q) ok = %v, want %v", tc.header, ok, tc.ok)
			}
			if !ok {
				if got != (progress.TraceContext{}) {
					t.Errorf("an invalid traceparent gave %+v, want nothing", got)
				}
				return
			}
			if got.Trace.String() != tc.wantTrace || got.Parent.String() != tc.wantParentID {
				t.Errorf("trace %s, parent %s; want %s and %s", got.Trace, got.Parent, tc.wantTrace, tc.wantParentID)
			}
			if got.Flags != tc.flags || got.State != tc.wantState {
				t.Errorf("flags %02x and state %q, want %02x and %q", got.Flags, got.State, tc.flags, tc.wantState)
			}
		})
	}
}

// Runs that continue one trace, as the commands of one CI step do from its
// TRACEPARENT, share its trace id and none of their span ids, and each
// says where the trace came from. A parent without a trace is none.
func TestRunsThatContinueOneTraceShareNoSpanID(t *testing.T) {
	t.Parallel()

	tc, ok := progress.ParseTraceContext("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "rojo=1")
	if !ok {
		t.Fatal("the traceparent was refused")
	}
	seen := map[progress.SpanID]int{}
	for run := range 2 {
		capture := &progresstest.Capture{}
		bus := progress.NewBus(progress.Options{
			Sinks: []progress.Sink{capture}, Trace: tc.Trace, Parent: tc.Parent, TraceFlags: tc.Flags, TraceState: tc.State,
		})
		if got := bus.TraceContext(); got != tc {
			t.Errorf("run %d continues %+v, want %+v", run+1, got, tc)
		}
		ctx := progress.WithBus(context.Background(), bus)
		ctx, cmd := progress.Start(ctx, progress.KindCommand, "exec")
		for range 1000 {
			_, s := progress.Start(ctx, progress.KindCall, "ssh")
			s.End(nil)
		}
		cmd.End(nil)
		bus.Close()
		for _, e := range capture.Events() {
			if e.Type != progress.TypeStart {
				continue
			}
			if other, ok := seen[e.Span]; ok {
				t.Fatalf("span id %s is run %d's and run %d's", e.Span, other+1, run+1)
			}
			seen[e.Span] = run
		}
	}

	bus := progress.NewBus(progress.Options{Parent: tc.Parent, TraceFlags: 1, TraceState: "rojo=1"})
	if got := bus.TraceContext(); got.Trace == (progress.TraceID{}) || got.Trace == tc.Trace || got.Parent != 0 || got.State != "" {
		t.Errorf("a parent without a trace gave %+v, want a trace of its own and nothing else", got)
	}
}
