<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0021 — Report progress as our own span-shaped events

Status: accepted

## Context

A command on many hosts says nothing until it ends. ssh reaches 16 nodes at a
time and Redfish 8, so a reinstall or a power-on of a few hundred machines
runs for minutes with no word on standard error. The one hook,
`fanout.Executor.OnResult`, has no start and never hears of the targets an
interrupt leaves out, so a counter built on it would stop short of its total.

What is wanted: a counter and a live tree on a terminal, plain lines for CI
when it asks, progress notifications for an agent over MCP, a log of events
for a bug report, and a way for the tests to hold every source of events to
one contract. Standard output must not change in any of them, and without
them standard error must not either: scripts read both, and under MCP the
command's standard error is the `notes` an agent is shown.

OpenTelemetry is the standard model for this: spans carried in a context,
and a tree built from their parents. Dagger draws its progress display from
it. A study of how, at Dagger v0.21.8, found three things that matter here:

- A span processor is told of a span when it starts and when it ends. What
  changes in between, a queued target that starts to run or a line of
  output, reaches a display only if the processor keeps the span and polls
  it, or if every change is sent again as a log record. Dagger takes a
  snapshot when a span starts and sees later changes only at its end, and
  sends output lines through the Logs API, which is not stable.
- The providers are meant to be process-wide, and making one reads the
  `OTEL_*` variables: a sampler has to be passed, or
  `OTEL_TRACES_SAMPLER=always_off` in the environment empties the display,
  and a malformed value is printed on standard error unless a process-wide
  error handler is set first.
- Queued work is invisible: Dagger's pool starts a job's span only when the
  job gets a place in it.

OpenTelemetry left the build with sops ([ADR 0019](0019-decrypt-with-the-sops-command.md)),
and is no longer in the module graph at all. The measurements of the design
this record follows were taken while sops still linked part of it, and are
not repeated here. Measured again, at the commit that added
`internal/progress`:

| | `internal/progress` | OpenTelemetry `sdk/trace` v1.46.0 |
| --- | --- | --- |
| Stripped release binary, linux/amd64, 16,732,322 B with neither | +69,632 B | +1,056,768 B |
| Packages linked, 329 with neither | +1 | +39 |
| Modules in the build graph, 52 with neither | none | +12 |
| Requirements added to `go.mod` | none | 9, three of them direct |
| A target's lifecycle: queued, run, one call, both ended | 1.7 µs and 8 allocations with a sink, which is sent 5 events; 19 ns and none without a Bus | 2.9 µs and 20 allocations with a span processor that does nothing |

The binaries are built as ADR 0019's were: `CGO_ENABLED=0 GOOS=linux
GOARCH=amd64 go build -trimpath -ldflags '-s -w' ./cmd/clusterctl`, on a copy
of the tree. Each variant is linked from an `init` that returns at once
unless a variable is set, so its code is in the binary but never runs. The
OpenTelemetry variant makes a private `TracerProvider` with `AlwaysSample`
and a span processor, and starts and ends two spans with an event and an
attribute: the least an event model on it needs, without an exporter. The
times are `BenchmarkTargetLifecycle` and the same steps on the SDK, on four
cores at 2.8 GHz.

## Decision

Progress is reported as events of our own, in `internal/progress`, which
depends on the standard library, `exitcode`, and `output` for escaping.

- **Spans in the context.** Work is a tree of spans at six levels: command,
  step, batch, target, call and wait. A span travels in the
  `context.Context` under a key of the package's own, never OpenTelemetry's,
  so a call nests under the target it is made for without being handed down.
- **Known work is announced first.** A target starts queued, before its pool
  runs it, and is marked running when it takes its place, so a display
  knows the whole of the work from the start, and every target ends however
  the command ends, so a counter reaches its total after an interrupt too.
- **Plain events, one order.** Every change is one `Event` of plain data,
  numbered and handed to the sinks of a `Bus` under one lock. Sinks are in
  the process: the display, the MCP notifier, an event log, a test. A sink
  updates memory and never blocks; one that panics is removed.
- **A closed vocabulary.** An event carries the fields of `progress.Fields`
  and nothing else. There is no field for an argument vector, a script,
  standard input, the environment or a header.
- **Remote bytes are data.** Every text in an event has been through
  `progress.Sanitize`: carriage returns are applied as a terminal shows
  them, `output.EscapeCell` escapes the rest, and the text is cut on a rune
  boundary. Lines of output are produced only when a sink asks for them and
  the command shows lines, and are rate limited, the newest kept. A parser
  is handed only lines that ended.
- **The terminal is lent, not shared.** `progress.Suspend` returns once every
  display is off the terminal, for the confirmation, a password prompt, sops
  and a credential helper to ask their questions.
- **Nothing for nobody.** Without a Bus in the context, `Start` returns a nil
  span that does nothing, and a target's lifecycle allocates nothing.
- **A trace, if one is ever wanted.** The trace id is 16 bytes and a span id
  8, drawn from a random base per Bus, so that runs sharing one trace do not
  share span ids. ok maps to OpenTelemetry's `Ok`, failed and canceled to
  `Error` with the error as description, and skipped to `Unset`. An exporter
  is a converter of the event log, built only when a site asks for one, in a
  module of its own. The main module imports nothing from
  `go.opentelemetry.io`, and a lint rule keeps it so.
- **Drawn only on a terminal.** Progress goes to standard error, never to
  standard output. `--progress` and `CLUSTERCTL_PROGRESS` choose the display,
  `auto` by default, which draws only when standard error is a terminal.
  Under MCP the flag and the variable are ignored and nothing is drawn: the
  agent is sent notifications.

How a pool reports its targets is part of [ADR 0022](0022-bounded-pools-and-power-batches.md).

## Why

- Every consumer is in the process. No trace context has to cross into
  another program.
- Two conditions have to hold, and are easier to guarantee in code we own
  than to verify in a dependency, the reasoning of ADRs
  [0002](0002-own-nodeset-engine.md) and [0005](0005-own-redfish-client.md):
  no target's end may be lost, or a counter stops short, and no byte from a
  node may act on the terminal. `progresstest.Check` holds every source to
  the first, and `FuzzSanitize` holds `Sanitize` to the second.
- Nothing is process-wide. A command, or an MCP tool call, has a Bus of its
  own, so two calls share no state, and no variable meant for some other
  program's traces can switch the display off.
- The costs in the table: a fifteenth of the binary growth, no new modules,
  so no Dependabot updates of display code
  ([ADR 0012](0012-dependabot.md)), and less time per target.
- Events of plain data can be checked in every command test, and a tree
  that folds targets into node sets does not depend on scheduling, so the
  tests can compare it.
- About 1,400 lines of our own, comments included and the displays not yet
  among them, are easier for a team that mostly writes Bash to read than an
  SDK ([ADR 0001](0001-go.md)).

## Costs

- The event model, the displays, the sanitiser and its fuzz target are ours
  to maintain, and a terminal UI among them has tmux, narrow terminals and
  resizes to cope with.
- There is no exporter and no propagation for free. Taking a trace id from
  `TRACEPARENT` and a converter to OTLP are work of our own when they are
  wanted.
- The helpers that reach hosts take a context, so that their spans nest
  under the step they belong to.
- A display that is not on a terminal has to be asked for, so CI shows
  progress only with `--progress=plain`.
- The vocabulary will be asked to grow. A field is added when a display
  needs it, not before.

## Reconsider when

- several tools at a site need their traces in one collector;
- clusterctl gains a second process that needs the trace context;
- a library clusterctl calls has to nest its own spans under ours;
- or the OpenTelemetry Logs API is stable and export becomes the main use.
