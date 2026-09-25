// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package progress reports what a command is doing while it runs.
//
// The work is a tree of spans, one level for each kind of unit: the command,
// its steps, the batches of a staggered action, the targets a step works on,
// the calls made for a target, and the waits in between. A span travels in
// the context.Context the work is done under, so the span a call starts
// nests under the target it is made for without being handed down. Every
// change to a span is sent, as one Event of plain data, to the sinks of the
// Bus the context carries, in one total order. A counter, a live tree, an
// agent's progress notifications and a test are sinks; this package draws
// nothing itself.
//
// Without a Bus in the context, Start returns the context unchanged and a
// nil *Span, and every method of a nil *Span does nothing, so work that
// nobody watches pays only for the lookup.
//
// The vocabulary is closed: an event carries the fields of Fields and
// nothing else, so an argument vector, a script, standard input or a header
// has nowhere to go. Text that came from elsewhere, an error or a remote
// line, is passed through Sanitize before any sink sees it.
//
// The events map one to one onto a trace: the trace id is 16 bytes, a span
// id is 8, and each Bus draws the base of its span ids at random, so that
// runs sharing one trace do not share span ids.
package progress

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// TraceID names the trace every span of one Bus belongs to.
type TraceID [16]byte

// String returns the id as 32 hexadecimal digits.
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// SpanID names a span. It is never zero, and unique within a trace; Seq,
// not the id, orders the events.
type SpanID uint64

// String returns the id as 16 hexadecimal digits, big-endian.
func (s SpanID) String() string { return fmt.Sprintf("%016x", uint64(s)) }

// Kind is the level of the tree a span is on.
type Kind uint8

const (
	// KindCommand is the command being run, the root of the tree.
	KindCommand Kind = iota + 1
	// KindStep is a named phase of a command, such as "reset the
	// machines". A step with Fold is where a pool's targets are counted.
	KindStep
	// KindBatch is one of the batches a staggered action runs in turn.
	KindBatch
	// KindTarget is one node, service processor, role, name or port that
	// a step works on.
	KindTarget
	// KindCall is one request made for a target: an ssh command, a
	// Redfish request, a copy, a lookup.
	KindCall
	// KindWait is time spent waiting on purpose: the confirmation, the
	// pause between batches.
	KindWait
)

func (k Kind) String() string {
	switch k {
	case KindCommand:
		return "command"
	case KindStep:
		return "step"
	case KindBatch:
		return "batch"
	case KindTarget:
		return "target"
	case KindCall:
		return "call"
	case KindWait:
		return "wait"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// State is where a span is in its life.
type State uint8

const (
	// StateQueued is a span announced before its work starts, such as a
	// target waiting for a place in its pool.
	StateQueued State = iota + 1
	// StateRunning is a span whose work has started.
	StateRunning
	// StateEnded is a span that has ended, however it ended.
	StateEnded
)

func (s State) String() string {
	switch s {
	case StateQueued:
		return "queued"
	case StateRunning:
		return "running"
	case StateEnded:
		return "ended"
	}
	return fmt.Sprintf("state(%d)", uint8(s))
}

// Status says how a span ended.
type Status uint8

const (
	// StatusOK is work that succeeded.
	StatusOK Status = iota + 1
	// StatusFailed is work that failed.
	StatusFailed
	// StatusCanceled is work that was interrupted, or never started
	// because of an interrupt.
	StatusCanceled
	// StatusSkipped is work left out on purpose, such as a call a dry run
	// does not send or a batch after one that failed.
	StatusSkipped
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusFailed:
		return "failed"
	case StatusCanceled:
		return "canceled"
	case StatusSkipped:
		return "skipped"
	}
	return fmt.Sprintf("status(%d)", uint8(s))
}

// Class says in a word why a span failed, for a display to group failures
// by. Classify tells it from the error.
type Class uint8

const (
	// ClassNone is a span that did not fail.
	ClassNone Class = iota
	// ClassTarget is a target that answered and said no, such as a
	// command that exited non-zero.
	ClassTarget
	// ClassTransport is a host that could not be reached or kept.
	ClassTransport
	// ClassTimeout is work that ran out of time.
	ClassTimeout
	// ClassAuth is an account that was refused.
	ClassAuth
	// ClassPin is a certificate that does not match the one pinned.
	ClassPin
	// ClassUsage is a request refused before anything was contacted.
	ClassUsage
	// ClassCanceled is work that was interrupted.
	ClassCanceled
)

func (c Class) String() string {
	switch c {
	case ClassNone:
		return "none"
	case ClassTarget:
		return "target"
	case ClassTransport:
		return "transport"
	case ClassTimeout:
		return "timeout"
	case ClassAuth:
		return "auth"
	case ClassPin:
		return "pin"
	case ClassUsage:
		return "usage"
	case ClassCanceled:
		return "canceled"
	}
	return fmt.Sprintf("class(%d)", uint8(c))
}

// Flags tell a display how to show a span.
type Flags uint8

const (
	// Hidden is plumbing, drawn only when it fails or takes long. A span
	// started under a Hidden one is Hidden too.
	Hidden Flags = 1 << iota
	// Fold marks a step whose finished targets a display folds into node
	// sets. A Fold step with no Fold step above it is where a counter
	// counts: its Total is what the counter expects.
	Fold
	// ShowLines lets the output of the calls below be shown as it
	// arrives, for exec, where the output is the product. A span started
	// under one shows lines too.
	ShowLines
	// DryRun marks the command of a dry run, and a call a dry run only
	// recorded. It is not passed down.
	DryRun
)

// inherited are the flags a span passes to the spans started under it.
const inherited = Hidden | ShowLines

func (f Flags) String() string {
	var names []string
	for _, n := range []struct {
		flag Flags
		name string
	}{{Hidden, "hidden"}, {Fold, "fold"}, {ShowLines, "show-lines"}, {DryRun, "dry-run"}} {
		if f&n.flag != 0 {
			names = append(names, n.name)
		}
	}
	return strings.Join(names, ",")
}

// Type is what an event reports.
type Type uint8

const (
	// TypeStart is a span that begins, queued or running.
	TypeStart Type = iota + 1
	// TypeRun is a queued span whose work has started.
	TypeRun
	// TypeUpdate is a span whose Total or Message changed.
	TypeUpdate
	// TypeLine is a line of output from the work of a call.
	TypeLine
	// TypeEnd is a span that ended; Status says how.
	TypeEnd
	// TypeSuspend asks every display to take itself off the terminal,
	// since something is about to ask a question there.
	TypeSuspend
	// TypeResume ends a TypeSuspend.
	TypeResume
)

func (t Type) String() string {
	switch t {
	case TypeStart:
		return "start"
	case TypeRun:
		return "run"
	case TypeUpdate:
		return "update"
	case TypeLine:
		return "line"
	case TypeEnd:
		return "end"
	case TypeSuspend:
		return "suspend"
	case TypeResume:
		return "resume"
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// Stream is the output stream a Line came from.
type Stream uint8

const (
	// Stdout is standard output.
	Stdout Stream = 1
	// Stderr is standard error.
	Stderr Stream = 2
)

func (s Stream) String() string {
	switch s {
	case Stdout:
		return "stdout"
	case Stderr:
		return "stderr"
	}
	return fmt.Sprintf("stream(%d)", uint8(s))
}

// The bounds of the text an event carries, in bytes. Sanitize cuts to them.
const (
	// MaxText bounds the text of a line of output.
	MaxText = 512
	// MaxErr bounds the error a span ended with.
	MaxErr = 1024
	// MaxField bounds a span's name, its Message and every other text
	// field but Node, which is a node set a display folds and is never
	// cut.
	MaxField = 256
)

// Fields is the whole vocabulary a span can say about itself. There is no
// bag of free-form attributes: a value without a field here cannot reach a
// sink, and so reaches no log or exporter either.
type Fields struct {
	// Node is what a display folds finished targets by: the node, the
	// service processor, the role, the name or the port. It may be a node
	// set, such as the nodes of a batch.
	Node string
	// Host is the address the work goes to, and Role its host role.
	Host, Role string
	// Total is how many targets to expect below a step or a batch. It
	// only ever grows: a smaller Total given later is ignored.
	Total int
	// Limit is how many of those targets are worked on at once.
	Limit int
	// Batch is "i/n", the place of a batch among its step's.
	Batch string
	// Message is one short line for a display, such as the question a
	// confirmation asks.
	Message string
	// Method and Path are the request of a Redfish call, and HTTPStatus
	// its answer.
	Method, Path string
	HTTPStatus   int
	// Cache says where a lookup was answered from: "hit", "miss",
	// "memory" or "disk".
	Cache string
	// Source is the kind of source a secret or a credential was read
	// from, such as "age", "sops", "prompt" or "command"; never the value.
	Source string
	// Timeout is the bound of a call, or the length of a wait.
	Timeout time.Duration
	// Exit is the exit code of a remote command, nil when there was none.
	Exit *int
}

// Event is one change to a span, or a suspension of the displays. It is plain data:
// no errors, buffers or maps, so a sink may keep it as it is.
type Event struct {
	// Seq numbers the events of a Bus from 1, with no gaps: the order in
	// which every sink sees them.
	Seq  uint64
	Time time.Time
	Type Type

	// Span is the span the event is about, and Parent the span it was
	// started under; zero for the root. A TypeSuspend or TypeResume names
	// the span it was asked for under, if any.
	Span, Parent SpanID
	Kind         Kind
	// Name says what the span is, in few words: "ssh", "redfish",
	// "reset the machines", or a target's name.
	Name  string
	Flags Flags
	// State is the span's state once the event has happened.
	State State
	// Fields are the span's fields once the event has happened.
	Fields

	// Status, Class and Err say how a TypeEnd ended; Err is one line.
	Status Status
	Class  Class
	Err    string

	// Stream and Text are the line of a TypeLine, sanitised.
	Stream Stream
	Text   string
	// Dropped counts the lines the sinks were not sent: on a TypeLine,
	// those left out since the span's previous line, and on a TypeEnd, all
	// the span's.
	Dropped int
}

// Sink receives the events of a Bus, one at a time, in Seq order, while the
// Bus is locked. It only updates memory, never blocks and never calls the
// Bus back; drawing happens on the sink's own time. A sink that panics is
// removed from the Bus.
type Sink interface {
	Handle(Event)
}

// LineSink is a Sink that may ask for the lines of output, which are only
// produced when a sink asks. The Bus asks once, when it is made.
type LineSink interface {
	WantsLines() bool
}

// Suspender is a Sink that draws on the terminal, and takes itself off it
// while something else asks a question there. Suspend and Resume are called
// outside the Bus lock, and Suspend returns once the terminal is clear: they
// are the only calls in which a sink may write to the terminal itself.
type Suspender interface {
	Suspend()
	Resume()
}

// Classifier is an error that says its own Class, such as a certificate
// that does not match its pin or an account a service processor refused.
// ClassNone leaves the error to the rules of Classify.
type Classifier interface {
	ProgressClass() Class
}

// Option sets what a span says about itself when it starts, changes or
// ends.
type Option func(*options)

type options struct {
	Fields
	flags  Flags
	queued bool
	// exit is Exit's value until a span takes it: taking the address of
	// the option's argument would cost an allocation even where nobody
	// watches.
	exit    int
	hasExit bool
}

// apply applies opts to o.
func (o *options) apply(opts []Option) {
	for _, opt := range opts {
		opt(o)
	}
	if o.hasExit {
		exit := o.exit
		o.Exit = &exit
	}
}

// Queued starts a span queued rather than running: announced now,
// worked on once Run is called. A pool starts every target this way before
// the first one runs, so that a display knows the whole of the work from
// the start.
func Queued() Option { return func(o *options) { o.queued = true } }

// WithFlags gives a span flags when it starts.
func WithFlags(f Flags) Option { return func(o *options) { o.flags |= f } }

// Node sets Node.
func Node(s string) Option { return func(o *options) { o.Node = s } }

// Host sets Host.
func Host(s string) Option { return func(o *options) { o.Host = s } }

// Role sets Role.
func Role(s string) Option { return func(o *options) { o.Role = s } }

// Total sets Total, which never shrinks.
func Total(n int) Option { return func(o *options) { o.Total = n } }

// Limit sets Limit.
func Limit(n int) Option { return func(o *options) { o.Limit = n } }

// Batch sets Batch to "i/n".
func Batch(i, n int) Option {
	return func(o *options) { o.Batch = fmt.Sprintf("%d/%d", i, n) }
}

// Message sets Message.
func Message(s string) Option { return func(o *options) { o.Message = s } }

// HTTP sets the Method and Path of a Redfish call.
func HTTP(method, path string) Option {
	return func(o *options) { o.Method, o.Path = method, path }
}

// HTTPStatus sets HTTPStatus.
func HTTPStatus(code int) Option { return func(o *options) { o.HTTPStatus = code } }

// Cache sets Cache.
func Cache(s string) Option { return func(o *options) { o.Cache = s } }

// Source sets Source.
func Source(s string) Option { return func(o *options) { o.Source = s } }

// Timeout sets Timeout.
func Timeout(d time.Duration) Option { return func(o *options) { o.Timeout = d } }

// Exit sets Exit.
func Exit(code int) Option { return func(o *options) { o.exit, o.hasExit = code, true } }
