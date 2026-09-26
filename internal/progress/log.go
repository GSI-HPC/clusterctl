// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package progress

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// LogVersion is the version of the event log's schema, the "v" of every
// line. It changes when what a key or a value means does, not when a key or
// a value is added: a reader leaves out the keys and the values it does not
// know.
const LogVersion = 1

const (
	// logBuffer is the most the event log writes at once, whole lines.
	logBuffer = 64 << 10
	// logBehind is how much of the event log may wait to be written before
	// the log stops, a writer that has stopped taking it.
	logBehind = 8 << 20
	// logFlushEvery is how long a line waits to be written at most.
	logFlushEvery = time.Second
	// logCloseWait is how long Close waits for what is left to be written.
	logCloseWait = 5 * time.Second
	// logTime is the time of a line, in UTC and always as long, so that the
	// times of one run sort as they happened.
	logTime = "2006-01-02T15:04:05.000000000Z07:00"
)

// Log is a sink that writes the events of a Bus as JSON, one line each: an
// event log, for a bug report, a test, or a converter to another tracing
// system.
//
// The first line is the run, the trace its events belong to, where the
// trace came from when another program handed it on, and the version of
// clusterctl that wrote it:
//
//	{"v":1,"type":"trace","run":"5d0c…a1f3","trace":"4bf9…4736","parent":"00f067aa0ba902b7","traceFlags":"01","traceState":"rojo=00f067aa0ba902b7","version":"v0.4.0"}
//
// and every line after it one event, with the run and the trace again, its
// Seq, Time and Type, the span's id and its parent's as 16 hexadecimal
// digits, its Kind, Name, Flags and State, the Fields it has, and how it
// ended:
//
//	{"v":1,"run":"5d0c…a1f3","trace":"4bf9…4736","seq":7,"time":"2026-09-26T12:00:03.000000000Z","type":"end","span":"9f3c…0007","parent":"9f3c…0002","kind":"target","name":"exe0002","state":"ended","node":"exe0002","host":"exe0002.hpc.example.org","status":"failed","class":"transport","err":"…"}
//
// The run tells the lines of one Log from those of others appending to the
// same file, which may share the trace. A Timeout is in seconds. A key whose
// value is zero or empty is left out. A line of output is written without
// its text, which never leaves the process: the event says only which
// stream it came from and how many lines before it were not sent; and since
// the log asks for no lines, there are line events only while a display
// asks for them.
//
// The lines are written by a goroutine of the log's own, never under the
// Bus's lock, in pieces of whole lines, so that the lines of runs appending
// to one file at once do not cut into each other: once 64 KiB wait, as a
// step, a batch or the command ends, and a second after a line came at the
// latest, so that a log followed as it grows, or one of a run killed, falls
// little behind. A write that fails stops the log, and so does a writer
// that falls 8 MiB behind, a pipe whose reader has stopped: the log never
// holds the work up. Close writes what is left, waiting five seconds at
// most, and says why the log stops short when it does.
type Log struct {
	w   io.Writer
	o   LogOptions
	run string

	mu      sync.Mutex
	line    bytes.Buffer
	enc     *json.Encoder
	trace   string
	pending []byte
	err     error
	closed  bool

	wake          chan struct{}
	stop, stopped chan struct{}
}

// errBehind stops a log whose writer has fallen too far behind.
var errBehind = errors.New("the lines were not written as fast as the work went on, and the rest were left out")

// LogOptions configure a Log.
type LogOptions struct {
	// Version is the version of clusterctl that writes the log, which its
	// first line names.
	Version string
	// Run names the run on every line; empty draws 16 hexadecimal digits
	// at random.
	Run string
	// FlushEvery is how long a line is kept at most before it is written;
	// zero is a second.
	FlushEvery time.Duration
	// CloseWait is how long Close waits for what is left to be written;
	// zero is five seconds.
	CloseWait time.Duration
}

// NewLog returns a Log that writes to w, and starts the goroutine that
// writes; Close stops it.
func NewLog(w io.Writer, o LogOptions) *Log {
	l := &Log{w: w, run: o.Run, o: o,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{})}
	if l.run == "" {
		var run [8]byte
		// crypto/rand never fails; it ends the process when it cannot.
		_, _ = rand.Read(run[:])
		l.run = hex.EncodeToString(run[:])
	}
	if l.o.FlushEvery <= 0 {
		l.o.FlushEvery = logFlushEvery
	}
	if l.o.CloseWait <= 0 {
		l.o.CloseWait = logCloseWait
	}
	l.enc = json.NewEncoder(&l.line)
	// What is escaped for HTML is nothing a log needs escaped.
	l.enc.SetEscapeHTML(false)
	go l.writeOut()
	return l
}

// logTrace is the first line of an event log.
type logTrace struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	Run        string `json:"run"`
	Trace      string `json:"trace"`
	Parent     string `json:"parent,omitempty"`
	TraceFlags string `json:"traceFlags,omitempty"`
	TraceState string `json:"traceState,omitempty"`
	Version    string `json:"version,omitempty"`
}

// logEvent is one event of an event log. Every member of Event is either
// copied into it by Handle or left out on purpose, as Text is.
type logEvent struct {
	V      int      `json:"v"`
	Run    string   `json:"run"`
	Trace  string   `json:"trace"`
	Seq    uint64   `json:"seq"`
	Time   string   `json:"time"`
	Type   string   `json:"type"`
	Span   string   `json:"span,omitempty"`
	Parent string   `json:"parent,omitempty"`
	Kind   string   `json:"kind,omitempty"`
	Name   string   `json:"name,omitempty"`
	Flags  []string `json:"flags,omitempty"`
	State  string   `json:"state,omitempty"`

	Node       string  `json:"node,omitempty"`
	Host       string  `json:"host,omitempty"`
	Role       string  `json:"role,omitempty"`
	Total      int     `json:"total,omitempty"`
	Limit      int     `json:"limit,omitempty"`
	Batch      string  `json:"batch,omitempty"`
	Message    string  `json:"message,omitempty"`
	Method     string  `json:"method,omitempty"`
	Path       string  `json:"path,omitempty"`
	HTTPStatus int     `json:"httpStatus,omitempty"`
	Cache      string  `json:"cache,omitempty"`
	Source     string  `json:"source,omitempty"`
	Timeout    float64 `json:"timeout,omitempty"`
	Exit       *int    `json:"exit,omitempty"`

	Status  string `json:"status,omitempty"`
	Class   string `json:"class,omitempty"`
	Err     string `json:"err,omitempty"`
	Stream  string `json:"stream,omitempty"`
	Dropped int    `json:"dropped,omitempty"`
}

// Begin writes the trace the events belong to, the log's first line.
func (l *Log) Begin(tc TraceContext) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trace = tc.Trace.String()
	line := logTrace{V: LogVersion, Type: "trace", Run: l.run, Trace: l.trace, Version: l.o.Version}
	if tc.Parent != 0 {
		line.Parent = tc.Parent.String()
		line.TraceFlags = fmt.Sprintf("%02x", tc.Flags)
		line.TraceState = tc.State
	}
	l.write(line)
}

// Handle writes e as a line of its own.
func (l *Log) Handle(e Event) {
	line := logEvent{
		V:     LogVersion,
		Seq:   e.Seq,
		Time:  e.Time.UTC().Format(logTime),
		Type:  e.Type.String(),
		Name:  e.Name,
		Flags: e.Flags.names(),

		Node:       e.Node,
		Host:       e.Host,
		Role:       e.Role,
		Total:      e.Total,
		Limit:      e.Limit,
		Batch:      e.Batch,
		Message:    e.Message,
		Method:     e.Method,
		Path:       e.Path,
		HTTPStatus: e.HTTPStatus,
		Cache:      e.Cache,
		Source:     e.Source,
		Timeout:    e.Timeout.Seconds(),
		Exit:       e.Exit,

		Err:     e.Err,
		Dropped: e.Dropped,
	}
	if e.Span != 0 {
		line.Span = e.Span.String()
	}
	if e.Parent != 0 {
		line.Parent = e.Parent.String()
	}
	if e.Kind != 0 {
		line.Kind = e.Kind.String()
	}
	if e.State != 0 {
		line.State = e.State.String()
	}
	if e.Status != 0 {
		line.Status = e.Status.String()
	}
	if e.Class != ClassNone {
		line.Class = e.Class.String()
	}
	if e.Stream != 0 {
		line.Stream = e.Stream.String()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	line.Run, line.Trace = l.run, l.trace
	l.write(line)
	if e.Type == TypeEnd && (e.Kind == KindCommand || e.Kind == KindStep || e.Kind == KindBatch) {
		l.due()
	}
}

// WantsLines says that the log asks for no lines of output: it never
// writes their text.
func (l *Log) WantsLines() bool { return false }

// write keeps v as a line, whole, for the goroutine that writes, and has it
// written once 64 KiB wait. l.mu is held.
func (l *Log) write(v any) {
	if l.err != nil || l.closed {
		return
	}
	l.line.Reset()
	if err := l.enc.Encode(v); err != nil {
		l.err = err
		return
	}
	if len(l.pending)+l.line.Len() > logBehind {
		l.err, l.pending = errBehind, nil
		return
	}
	l.pending = append(l.pending, l.line.Bytes()...)
	if len(l.pending) >= logBuffer {
		l.due()
	}
}

// due has the lines kept written now. l.mu is held.
func (l *Log) due() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// writeOut writes the lines kept as they fall due, until Close.
func (l *Log) writeOut() {
	defer close(l.stopped)
	tick := time.NewTicker(l.o.FlushEvery)
	defer tick.Stop()
	for {
		select {
		case <-l.stop:
			l.drain()
			return
		case <-l.wake:
		case <-tick.C:
		}
		l.drain()
	}
}

// drain writes the lines kept, in pieces of whole lines of at most 64 KiB,
// a longer line on its own. The first write that fails stops the log.
func (l *Log) drain() {
	l.mu.Lock()
	lines := l.pending
	l.pending = nil
	l.mu.Unlock()
	for len(lines) > 0 {
		n := len(lines)
		if n > logBuffer {
			n = bytes.LastIndexByte(lines[:logBuffer], '\n') + 1
			if n == 0 {
				n = bytes.IndexByte(lines, '\n') + 1
			}
		}
		if _, err := l.w.Write(lines[:n]); err != nil {
			l.mu.Lock()
			if l.err == nil {
				l.err = err
			}
			l.pending = nil
			l.mu.Unlock()
			return
		}
		lines = lines[n:]
	}
}

// Close writes what is left of the log, once the Bus is closed, waiting
// CloseWait at most, and returns the first error the log met: nothing
// after it was written. The writer is not closed; that is for whoever
// opened it to do. Closing a closed Log only says so again.
func (l *Log) Close() error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.stop)
	}
	l.mu.Unlock()
	select {
	case <-l.stopped:
	case <-time.After(l.o.CloseWait):
		l.mu.Lock()
		if l.err == nil {
			l.err = fmt.Errorf("the last lines were not written within %s", l.o.CloseWait)
		}
		l.mu.Unlock()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}
