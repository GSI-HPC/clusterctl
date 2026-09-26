---
title: Progress
weight: 8
---

A command on many hosts can run for minutes. While it runs, clusterctl shows
on standard error how far it has got: which step it is in, how many of the
nodes, service processors, names or ports it works on are done, which failed
and why, and which are still running. Standard output never carries any of
it, whatever `-o` says, so a pipe into `jq` reads what it would without
progress.

## What is shown

`--progress`, or `CLUSTERCTL_PROGRESS` when the flag is not given, chooses;
an empty variable is `auto`:

| Value | Shows |
| --- | --- |
| `auto`, the default | The live tree when standard error is a terminal, `TERM` is not `dumb` and standard output does not go into a pipe, and nothing otherwise |
| `tty` | The live tree, or the counter on a terminal too small for it; where standard error is not a terminal or `TERM` is `dumb`, refused when `--progress` asks for it, and nothing when the variable does |
| `counter` | The counter; where standard error is not a terminal or `TERM` is `dumb`, refused when `--progress` asks for it, and nothing when the variable does |
| `plain` | Plain lines, with or without a terminal |
| `none` | Nothing |

`auto` draws nothing while standard output goes into a pipe, as in
`clusterctl exec … | grep load`: what reads it writes to the same terminal,
and nothing keeps its lines and the tree apart. `--progress tty` draws the
tree there all the same.

Without a terminal, in a pipe, a file or a CI log, nothing is shown unless
`plain` is asked for: standard error holds exactly what it holds with
`--progress none`, byte for byte, so a script that reads it is not surprised.
A display `--progress` asks for where it cannot be drawn is refused, exit code
2, rather than dropped without a word. `CLUSTERCTL_PROGRESS` is set once, in a
profile, and inherited by cron jobs and CI steps that never asked for
anything, so it fails no command: the tree or the counter it asks for where
neither can be drawn shows nothing, as `auto` would, and a value it does not
take shows nothing, with one line on standard error that says so. The
commands that hand the terminal to another
program, such as `login`, the shells and `tunnel start`, show nothing, and
neither do the commands an agent runs through `clusterctl mcp`, which tells
the agent instead. Whatever is shown, the events can be kept in an
[event log](#the-event-log) too.

The tree and the counter draw nothing in a command's first second, so a
command that is done by then leaves the terminal as it would without them.

## The live tree

A command that has run for a second on a terminal shows the work under way as a
live tree, a few rows at the bottom of standard error redrawn at most ten times
a second:

```console
✓ configuring the network boot  2.3s
provision reinstall · 4:12
  setting the machines to boot from the network once  312/480 · 1 failed · 8 running · 159 queued
    ✗ exe0007  transport: {}: dial tcp: i/o timeout
    ▸ exe0313  4s  PATCH /redfish/v1/Systems/1
    ▸ exe0314  3s  GET /redfish/v1/Systems/1
    … 6 more running
    ✓ exe[0001-0006,0008-0312]
```

The first row names the command and says how long it has run. Each step under
way has a row under it: how many of its targets are done, however they ended,
of how many; how many of those failed, were interrupted or were left out; and
how many run now and how many wait their turn. Under a step, the targets that
failed alike share a row, with the class of the failure and the error, each
target's own name written `{}`; those interrupted and those left out get a row
each; those running are listed, the longest running first, with how long they
have run, against the bound of the request they wait for when it has one
(`3m12s/10m`), and what that request is, or, for `exec` and `cinc run`, the
last line the node printed; and those that are done are one node set. A batch
of a staggered action has a row of its own under the step, and the pause
before the next one counts down. The lookups a command makes before it asks,
such as reading a file from a host or running a group source's command, are
drawn only once they have taken a second. The tree takes a third of the
terminal at most, and six on a small one; the running targets that do not fit
are counted instead, `… 6 more running`, and a row is cut at the right edge.
Once a step is over, it leaves a line above the tree with how long it took and
how its targets ended, and the failures under it; a step that succeeded at
once, with nothing under it, leaves none.

The class says in a word why a target failed: `target` when it answered and
said no, such as a command that exited non-zero; `transport` when it could not
be reached or kept; `timeout`; `auth` for an account a service processor
refused; `pin` for a certificate that is not the one recorded; `usage` for a
request refused before anything was sent; and `canceled` for work an
interrupt stopped.

The tree is taken off before anything else is written and drawn again below
it, so a note the command prints, such as the fallback from Redfish to IPMI,
lands above it and stays; and it is gone before the command ends.

## The counter

On a terminal of fewer than 8 rows or 40 columns, as it is at each redraw, the
counter is drawn instead, and `--progress counter` always draws it: one line at
the bottom of standard error that says how far the work has got:

```console
power on · batch 3/60 · 17/480 · 1 failed · 8 running · 0:41
```

The line names each step that works on many targets: how many of its targets
are done, however they ended, of how many; how many of those failed, were
interrupted or were left out; how many run now and how many wait their turn;
`waiting` during the pause between two batches; and, last, how long the
command has run. Steps under way side by side each get a part of the line,
split by `|`. While no such step runs, the line names the step or the command
that does. It comes and goes the way the tree does, and leaves the same summary
behind.

Neither the tree nor the counter draws in colour or hides the cursor, so a
command killed while it draws leaves a terminal that works. Outside a UTF-8
locale, as `LC_ALL`, `LC_CTYPE` or `LANG` names it, they draw with ASCII
alone: `+` for done, `x` for failed, `>` for running and ` - ` between the
parts of a row.

## Plain lines

Plain lines are for a log, a CI job's for instance, as much as for a terminal:
a line for each thing worth one, with the time since the command started in
front, and no escape codes:

```console
[0:00] bmc power › power off: start, 480 hosts, 8 at a time
[0:03] bmc power › power off › exe0007 failed (transport): exe0007.mgmt: dial tcp: i/o timeout
[0:10] bmc power › power off: 312/480 done, 1 failed, 8 running, 160 queued
[0:18] bmc power › power off: failed in 18s: 478 ok, 2 failed
clusterctl: bmc power: failed in 18s: 478 ok, 2 failed
```

A line names a step by its path from the command. There is one as a step or a
batch starts and as it ends, with how its targets ended; one as a pause between
two batches starts; one for each target that fails, once, with why; and, every
ten seconds, one for each step under way that works on many targets, with how
far it has got. The lookups a command makes before it asks, the single
requests and what the hosts print get none: `exec` prints the output at the
end, and `-o json` and `-o yaml` carry it whole. The lines are written to
standard error ahead of whatever the command writes after the work they tell
of, never into the middle of a line, and not while a question waits for its
answer. A CI job asks for them with `CLUSTERCTL_PROGRESS=plain` in its
environment. Outside a UTF-8 locale the parts of a path are split by `>`
rather than `›`.

## The summary

Once the tree, the counter or the plain lines are done, a command that ran for
a second or longer leaves one line more, just before its error:

```console
clusterctl: exec: failed in 1m12s: 478 ok, 2 failed
clusterctl: 2 of 480 hosts failed: exe[0007,0311]
```

It says how the command ended, `done`, `failed` or `canceled`, and how long it
ran, and counts each node once, as the worst of the ways it fared in the
command's steps that count their targets; what failed and why is the error's
to say, so the summary never repeats it. A command that works on no targets
says only how it ended. A command done within its first second leaves none,
whatever its targets did, and with `--progress none` there is none either.

The tree, the counter, the plain lines and the summary are for people to
read, and may change from one release to the next. A program that wants to
know what a command did, target by target, reads the
[event log](#the-event-log), whose format is versioned.

## Questions, passwords and Ctrl-C

The tree and the counter are taken off the terminal, and plain lines held
back, while a question waits for its answer: the confirmation, a password
asked for, a credential helper that can reach the terminal, as gpg's pinentry
does, and sops looking for its keys there. They come back once it has been
answered. A command put in the background, with `&` or Ctrl-Z and `bg`,
draws neither while it is there, since the shell's prompt shares its rows,
and draws again once it is back in the foreground. ssh can still ask a
question on the terminal by itself, a
passphrase for instance, which clusterctl does not see; if the tree or the
counter is drawn over it, the question still waits for its answer, and
`--progress none` leaves the terminal to ssh.

After Ctrl-C, the first row of the tree says `interrupting`, with how many of
the targets running will stop and how many of those waiting will not start:

```console
exec · interrupting · 3 running will stop · 5 queued will not start · 0:02
```

No new target starts, the requests under way are stopped, ssh among them, and
every target still open ends as interrupted, so the counts reach their totals; the summary counts them
`canceled`, and the command exits 130. The commands already running on the
nodes are not stopped; their timeout ends them. A second Ctrl-C ends
clusterctl at once, wherever it is: the rows drawn last may stay on the
screen, and the terminal works.

## What arrives as it happens

Each target is shown as it happens: queued, running, done, and how it ended,
with how long it took. That is what progress promises. A node's ssh command,
a service processor's answer over Redfish, each processor ipmitool or
ipmipower answers for, each fabric port and each name ends its target as its
answer arrives, not when the whole command does.

What a host prints is another matter. `exec` and `cinc run` show the last
line each running node printed, in the tree only, but a line reaches
clusterctl only when the program on the node writes it out, and most programs
keep what they write to a pipe, which ssh without a terminal gives them, until
a buffer of a few KiB is full or they exit. So a line may arrive late, in a
burst, or only as the command ends. clusterctl shows the newest line of each
node, at most ten a second after the first twenty, and never loses the last
one; `stdbuf -oL` in front of the command, where the program uses the C
library's buffering, has it write line by line. The output itself is printed
whole when the command ends, whatever was shown.

## The event log

`--progress-log FILE`, or `CLUSTERCTL_PROGRESS_LOG` when the flag is not
given, appends everything a command reports about its progress to `FILE`, one
JSON object per line, whatever `--progress` shows, `none` included. It is for
a bug report, a CI job's artifacts or a program that wants to know what a
command did, target by target; standard output and standard error stay as
they are.

```console
$ clusterctl --progress-log run.jsonl exec -n 'exe[1-3]' -y -- uptime
$ jq -c 'select(.type == "end" and .kind == "target") | [.name, .status, .class]' run.jsonl
["exe0001","ok",null]
["exe0002","failed","target"]
["exe0003","ok",null]
```

A command's first line, of `type` `trace`, names its run, the trace its
events belong to and the version of clusterctl that wrote it, and every line
after it is one event:

```json
{"v":1,"type":"trace","run":"5d0c9e7b2a41f386","trace":"8d5ef2a0c3a64b7e9f0d1c2b3a495867","version":"v0.4.0"}
{"v":1,"run":"5d0c9e7b2a41f386","trace":"8d5ef2a0c3a64b7e9f0d1c2b3a495867","seq":13,"time":"2026-09-26T12:00:05.012345678Z","type":"end","span":"b7e3a1c09d2f4e10","parent":"b7e3a1c09d2f4e0e","kind":"target","name":"exe0002","flags":["show-lines"],"state":"ended","node":"exe0002","host":"exe0002.hpc.example.org","status":"failed","class":"target","err":"exe0002 (exe0002.hpc.example.org): command exited 1"}
```

The first line holds `v`, `type`, `run`, `trace` and `version`, and, when the
command continues a trace another program began, `parent`, the span of that
program's it runs under, `traceFlags` and `traceState`, as they were given.
An event holds these keys:

| Key | Holds |
| --- | --- |
| `v` | The version of the format, `1` |
| `run` | The command that wrote the line, 16 hexadecimal digits drawn for each command, the same on all of its lines. Commands appending to one file may share a trace, never a run. |
| `trace` | The trace, 32 hexadecimal digits, the same on every line of one command |
| `seq`, `time` | The event's number, from 1 with no gaps within a run, and when it happened, in UTC, as RFC 3339 with nine digits after the second, so that the times of one run sort as they happened |
| `type` | `start`; `run`, a target that waited for its turn starts; `update`; `line`; `end`; `suspend` and `resume`, the display taken off the terminal for a question and put back |
| `span`, `parent` | The id of the span the event is about, and of the one it runs under, 16 hexadecimal digits; the command's span has no parent |
| `kind` | `command`, `step`, `batch`, `target`, `call` or `wait` |
| `name`, `flags`, `state` | What the span is; how a display shows it, `hidden`, `fold`, `show-lines` or `dry-run`; and `queued`, `running` or `ended` |
| `node`, `host`, `role` | The node, service processor, name or port a target or call is for, and the address and role it goes to |
| `total`, `limit`, `batch` | How many targets a step or batch expects and how many it works on at once, and a batch's place, `2/5` |
| `message` | A line for a display, such as the question a confirmation asks |
| `method`, `path`, `httpStatus` | A Redfish request, and the status of its answer |
| `cache`, `source` | Where a lookup was answered from, and the kind of source a credential or secret was read from, never its value |
| `timeout`, `exit` | The bound of a call or the length of a pause, in seconds, and the exit code of a remote command |
| `status`, `class`, `err` | How a span ended: `ok`, `failed`, `canceled` or `skipped`; the [class](#the-live-tree) of a failure, `target`, `transport`, `timeout`, `auth`, `pin`, `usage` or `canceled`; and the error, on one line |
| `stream`, `dropped` | The stream a line came from, `stdout` or `stderr`, and how many lines before it, or in all at the end, a display was not shown |

`name`, `message` and `err` are English for people to read, such as `resolve
the names` or `read /etc/dhcp/dhcpd.conf`, not names to match: they may change
from one release to the next. The other values are the ones the table lists.
The version changes only when what a key or a value means does: keys and
values may be added under version 1, so a reader leaves out those it does not
know rather than failing on them. `internal/progress/testdata/log-v1.jsonl` is
version 1, line for line, with every value of every key the table lists.

A key with nothing to say is left out. A span can say only what the table
lists, so the argument vector, a script, standard input, the environment and
the value of a credential or a secret never reach the log, and neither does
what a host prints: a `line` event, which there is only while the live tree
asks for the lines, says which stream the line came from and nothing of what
it said. An error can quote a host, escaped as it is on the terminal.

The log names hosts and says why they failed, so it is created readable and
writable by you alone. A file that is there already has to be yours, and
nobody else's to read or write, and a named pipe, and every symbolic link on
the way to the file, yours or root's, or it is not written: when `--progress-log`
names it, the command is refused before anything has run and told how to make
it so; when `CLUSTERCTL_PROGRESS_LOG` does, the command runs without a log,
and one line on standard error says why. A terminal or another device, and
a pipe of yours, such as `>(jq …)`, are written as they are. `/dev/stderr`,
`/dev/stdout` and `/dev/fd/N` are the command's own streams, wherever the
shell pointed them: with `2>run.log`, the events go into `run.log` between
the lines the command writes on standard error, and none is written over.
Each command appends, after its own first
line, so a CI job can set `CLUSTERCTL_PROGRESS_LOG` once for all its
commands; lines of two commands writing at once do not cut into each other on
a local file system, and each line's `run` says which command wrote it. An
empty `--progress-log` writes no log, whatever the variable says.

The events are written in whole lines, within a second of each, at once as a
step, a batch or the command ends, and the rest as the command ends, after
Ctrl-C too; `tail -f` follows a long command as it goes. A command killed, by
a second Ctrl-C or a CI job's timeout, loses at most its last second of
events. A log that cannot be written, on a full disk for instance, fails
nothing: the command ends as it would have, and one line on standard error
says the log stops short. Neither does one written more slowly than the work
goes, a pipe whose reader has stopped: once 8 MiB wait, the log stops, and the
command goes on as fast as it would without it. The commands that hand the
terminal to another program report no progress and write nothing, and neither
do the commands an agent runs through `clusterctl mcp`.

When `TRACEPARENT` holds a valid [W3C trace context](https://www.w3.org/TR/trace-context/),
as a CI system that traces its jobs may set it, the command continues that
trace: its events carry the trace's id, and its first line also names the
span it runs under, the trace flags and `TRACESTATE`, as they were given:

```json
{"v":1,"type":"trace","run":"5d0c9e7b2a41f386","trace":"4bf92f3577b34da6a3ce929d0e0e4736","parent":"00f067aa0ba902b7","traceFlags":"01","traceState":"ci=build-42","version":"v0.4.0"}
```

Commands that continue one trace share its id and never a span's or a run's.
A `TRACEPARENT` that is not valid is ignored, and whether the trace is
sampled changes nothing: the log is written either way. A valid one is
continued whoever set it, and nothing but the first line says so; run
`TRACEPARENT= clusterctl …` for a trace of the command's own. clusterctl
sends no trace anywhere, and hands none on: it takes `TRACEPARENT` and
`TRACESTATE` out of the environment of the programs it runs, ssh, scp, sops
and a credential helper, since what they trace would hang under a span that
no tracing system holds. The log is what a converter to a tracing system
would read.

## Progress for an agent

An agent's MCP client that asks for the progress of a tool call, with a
progress token, is sent MCP progress notifications as the call runs: how many
of the nodes, names or ports the call works on are done, of how many, and a
line such as `read the groups: 3/16 done, 1 failed`, which ends with what the
numbers count in all, as in `read the uptime: 0/6 done; 6/12 in all`, once
they count more than that step. They are sent at most every half second and as
each step ends, each saying more are done than the one before, and all of them
before the call's result; a call that counts nothing, or whose client gave no
token, is sent none. Nothing is drawn, `--progress` and `--progress-log`
cannot be given, `CLUSTERCTL_PROGRESS`, `CLUSTERCTL_PROGRESS_LOG` and
`TRACEPARENT` in the server's environment are not read for a call, and each
call is a trace of its own, which its lines in the audit log name. See
[Working with an agent](../agents/).
