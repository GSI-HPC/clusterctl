// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress

import "encoding/hex"

// TraceContext says which trace the spans of a Bus belong to and, when
// another program handed the trace on, where in it they belong: under a
// span of that program's, with the trace flags and the tracestate it gave.
// A Bus records where its trace came from, for an event log, and changes
// nothing else by it: a trace the other program does not sample is shown
// and logged all the same.
type TraceContext struct {
	Trace TraceID
	// Parent is the other program's span the work runs under; zero when
	// the trace began with the Bus.
	Parent SpanID
	// Flags are the trace flags the other program gave, whose lowest bit
	// says that it samples the trace.
	Flags byte
	// State is the tracestate the other program gave, the part of the
	// trace context each tracing system keeps for itself, as it was given.
	State string
}

// maxTraceState bounds the tracestate a Bus records: W3C asks that at
// least 512 characters of it be passed on.
const maxTraceState = 512

// ParseTraceContext reads the trace context another program handed on, as
// a traceparent and a tracestate in the W3C Trace Context format, such as
// the TRACEPARENT and TRACESTATE a CI job runs clusterctl with.
//
// A traceparent is four fields of lowercase hexadecimal digits split by
// dashes: the version, the trace id, the parent's span id and the trace
// flags, as in 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01.
// Version 00 is exactly that; a later version may add fields after a
// further dash, which are left out. Version ff, a trace or span id of
// zeros, a letter in upper case and anything else that does not fit make
// the traceparent invalid, and an invalid one is no trace context at all:
// ok is false, and the tracestate is not looked at. The tracestate is kept
// as it was given when it is at most 512 bytes of printable ASCII, and
// left out otherwise.
func ParseTraceContext(traceparent, tracestate string) (tc TraceContext, ok bool) {
	// "00-" + 32 digits + "-" + 16 digits + "-" + 2 digits.
	const size = 55
	s := traceparent
	if len(s) < size || s[2] != '-' || s[35] != '-' || s[52] != '-' {
		return TraceContext{}, false
	}
	version, ok := lowerHex(s[0:2])
	if !ok || version[0] == 0xff {
		return TraceContext{}, false
	}
	// Version 00 has nothing after the flags; a later one may, after a
	// dash.
	if (version[0] == 0 && len(s) != size) || (len(s) > size && s[size] != '-') {
		return TraceContext{}, false
	}
	trace, okTrace := lowerHex(s[3:35])
	parent, okParent := lowerHex(s[36:52])
	flags, okFlags := lowerHex(s[53:55])
	if !okTrace || !okParent || !okFlags {
		return TraceContext{}, false
	}
	copy(tc.Trace[:], trace)
	for _, b := range parent {
		tc.Parent = tc.Parent<<8 | SpanID(b)
	}
	if tc.Trace == (TraceID{}) || tc.Parent == 0 {
		return TraceContext{}, false
	}
	tc.Flags = flags[0]
	if validTraceState(tracestate) {
		tc.State = tracestate
	}
	return tc, true
}

// lowerHex decodes s, which has to be hexadecimal digits in lower case.
func lowerHex(s string) ([]byte, bool) {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, false
		}
	}
	b, err := hex.DecodeString(s)
	return b, err == nil
}

// validTraceState reports whether a tracestate may be recorded as it is:
// printable ASCII, which is all W3C allows in one, and not too long.
func validTraceState(s string) bool {
	if len(s) > maxTraceState {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}
