---
title: Output and scripting
weight: 7
---

Every command produces the same information in several shapes, and `-o` picks
one. A command never formats its own output, so `-o json` means the same thing
everywhere.

## Formats

| `-o` | Use |
| --- | --- |
| `table` | The default. Human readable, aligned, never truncated. |
| `wide` | The same plus the columns left out for width. |
| `json` | Structured, indented. |
| `yaml` | The same as YAML. |
| `nodeset` | Just the node set, folded. |
| `name` | One host name per line. |
| `jq=…` | A jq program, embedded; no jq binary needed. |

```console
$ clusterctl slurm node list --state drain -o nodeset
exe[0007,0042,0511]

$ clusterctl node list -o json | jq '.[] | select(.rack == "R02") | .name'

$ clusterctl bmc status -n '@rack:R02' -o jq='[.[].state] | join(" ")'
On On Off On

$ clusterctl exec -n '@compute' -o jq='.[] | select(.exitCode != 0) | .target.name' -- true
```

A `jq` program is checked when the command line is read. A mistake in it exits
2 before anything has run, rather than after a command has changed something
and has only its result left to print.

### jq

A `jq` program runs inside clusterctl and stops when the command is
interrupted. It cannot read the environment: `$ENV` and `env` are empty.
A string result prints without quotes, as `jq -r` would print it.

There is no `jsonpath` format; jq does what its templates do. jq prints each
value on a line of its own, and `null` for one that is missing:

| JSONPath | jq |
| --- | --- |
| `{.[*].name}` | `[.[].name] \| join(" ")` |
| `{.[0].name}`, `{.[-1].name}` | `.[0].name`, `.[-1].name` |
| `{.[1:3].name}` | `.[1:3][].name` |
| `{range .[*]}{.name}{"\n"}{end}` | `.[].name` |
| `node {.[0].name} in {.[0].rack}` | `"node \(.[0].name) in \(.[0].rack)"` |
| `{.[?(@.rack=="R02")].name}` | `.[] \| select(.rack == "R02") \| .name` |

```console
$ clusterctl exec -n '@compute' -o jq='.[] | "\(.target.name) \(.exitCode)"' -- true
```

### JSON and YAML

Numbers print as they are: an exit code of 0 is `0`, not `0.0`, and a large
PID keeps every digit, in YAML and in `jq` too. The YAML leaves a string
unquoted only when no YAML reader could take it for anything else, so `yes`,
`.inf`, `2026-09-24` and `10.0.0.1` come out quoted, and a control character is
written as an escape.

The result of a remote command, as `exec`, `copy` and `provision` print it,
looks like this; `error` appears only when the command failed:

```json
{
  "target": {"name": "exe0003", "host": "exe0003.hpc.example.org", "user": "alice_adm"},
  "exitCode": 3,
  "stdout": "failed\n",
  "error": "exe0003 (exe0003.hpc.example.org): command exited 3"
}
```

{{< callout type="info" >}}
Table output is never truncated. A long drain reason wraps rather than being
cut off, because the important half of a message is usually the end of it. The
last column is not padded, so a copied line carries no trailing spaces.
{{< /callout >}}

A value in a table may come from a node, a BMC or a Slurm user, so it is
escaped before it is printed: a newline, carriage return or tab shows as `\n`,
`\r` or `\t`, and any other control character as an escape such as `\x1b`.
So do a byte that is not UTF-8, the Unicode controls that change the direction
text is shown in, such as `\u202e`, and the line and paragraph separators
`\u2028` and `\u2029`. A value cannot start a row of its own, show itself in
another order than it has or move the cursor over what is already on the
screen. The `json` and `yaml` formats replace a byte that is not UTF-8 with
U+FFFD. The `yaml` format escapes every other one of these characters in its
own syntax. The `json` format escapes the C0 control characters and the line
and paragraph separators, but leaves DEL, the C1 control characters and the
direction controls in a string as they are, as JSON allows; a program that
shows a string from it on a terminal has to escape it. `jq` prints a selected
string as it is, like `jq -r`.

## Progress and errors go to stderr

Everything a program would parse goes to standard output; notes, progress and
prompts go to standard error. Redirecting one does not lose the other.

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

After Ctrl-C, the first row says `interrupting`, with how many of the targets
running will stop and how many of those waiting will not start.

The tree is taken off before anything else is written and drawn again below
it, it stays off while a question waits for its answer, and it is gone before
the command ends, so what is left on the terminal is the lines of the steps,
what the command printed, and one line more for a command that ran for
a second or longer or whose targets did not all succeed:

```console
provision reinstall: 478 ok, 2 failed in 18m03s
```

It counts each node once, as the worst of the ways it fared in the command's
steps, and says how long the command ran; the error after it says which nodes
failed and why.

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

Neither draws in colour or hides the cursor, so a command killed while it
draws leaves a terminal that works. Outside a UTF-8 locale, as `LC_ALL`,
`LC_CTYPE` or `LANG` names it, they draw with ASCII alone: `+` for done, `x`
for failed, `>` for running and ` - ` between the parts of a row.

Plain lines are for a log, a CI job's for instance, as much as for a terminal:
a line for each thing worth one, with the time since the command started in
front, and no escape codes:

```console
[0:00] bmc power › power off: start, 480 hosts, 8 at a time
[0:03] bmc power › power off › exe0007 failed (transport): exe0007.mgmt: dial tcp: i/o timeout
[0:10] bmc power › power off: 312/480 done, 1 failed, 8 running, 160 queued
[0:18] bmc power › power off: failed in 18s: 478 ok, 2 failed
bmc power: 478 ok, 2 failed in 18s
```

A line names a step by its path from the command. There is one as a step or a
batch starts and as it ends, with how its targets ended; one as a pause between
two batches starts; one for each target that fails, once, with why; and, every
ten seconds, one for each step under way that works on many targets, with how
far it has got. The lookups a command makes before it asks, the single
requests and what the hosts print get none: `exec` prints the output at the
end, and `-o json` and `-o yaml` carry it whole. The summary comes last, as it
does after the tree. The lines are written to standard error ahead of
whatever the command writes after the work they tell of, never into the middle
of a line, and not while a question waits for its answer.

`--progress`, or `CLUSTERCTL_PROGRESS` when the flag is not given, chooses what
is shown:

| Value | Shows |
| --- | --- |
| `auto`, the default | The live tree when standard error is a terminal and `TERM` is not `dumb`, and nothing otherwise |
| `tty` | The live tree, or the counter on a terminal too small for it; refused when standard error is not a terminal or `TERM` is `dumb` |
| `counter` | The counter; refused when standard error is not a terminal or `TERM` is `dumb` |
| `plain` | Plain lines, with or without a terminal |
| `none` | Nothing |

Without a terminal, in a pipe, a file or a CI log, nothing is shown unless
`plain` is asked for: standard error holds exactly what it holds with
`--progress none`. The format given with `-o` changes nothing here, since
progress never touches standard output. scp's own progress meter is left off
while progress is shown. The commands that hand the terminal to another
program, such as `login` and the shells, show nothing, and neither do the
commands an agent runs through `clusterctl mcp`. ssh can still ask a question
on the terminal by itself, a passphrase for instance, which clusterctl does not
see; if the tree or the counter is drawn over it, the question still waits for
its answer, and `--progress none` leaves the terminal to ssh.

An error message often quotes what a node, a BMC or a group source said, so it
is escaped the same way before it is printed, except that newlines and tabs are
kept: some messages are several lines on purpose, such as the list of problems
clusterctl found in a configuration file.

### The event log

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

A command's first line names the trace its events belong to, and every line
after it is one event:

```json
{"v":1,"type":"trace","trace":"8d5ef2a0c3a64b7e9f0d1c2b3a495867"}
{"v":1,"trace":"8d5ef2a0c3a64b7e9f0d1c2b3a495867","seq":13,"time":"2026-09-26T12:00:05Z","type":"end","span":"b7e3a1c09d2f4e10","parent":"b7e3a1c09d2f4e0e","kind":"target","name":"exe0002","flags":["show-lines"],"state":"ended","node":"exe0002","host":"exe0002.hpc.example.org","status":"failed","class":"target","err":"exe0002 (exe0002.hpc.example.org): command exited 1"}
```

| Key | Holds |
| --- | --- |
| `v` | The version of the format, `1`. Keys may be added; the version changes only when what one means does. |
| `trace` | The trace, 32 hexadecimal digits, the same on every line of one command |
| `seq`, `time` | The event's number, from 1 with no gaps, and when it happened, in UTC |
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
| `status`, `class`, `err` | How a span ended: `ok`, `failed`, `canceled` or `skipped`; the class of a failure, `target`, `transport`, `timeout`, `auth`, `pin`, `usage` or `canceled`; and the error, on one line |
| `stream`, `dropped` | The stream a line came from, and how many lines before it, or in all at the end, a display was not shown |

A key with nothing to say is left out. A span can say only what the table
lists, so the argument vector, a script, standard input, the environment and
the value of a credential or a secret never reach the log, and neither does
what a host prints: a `line` event, which there is only while the live tree
asks for the lines, says which stream the line came from and nothing of what
it said. An error can quote a host, escaped as it is on the terminal.

The log names hosts and says why they failed, so it is created readable and
writable by you alone. A file that is there already has to be yours, and
nobody else's to read or write, or the command is refused before anything has
run and told how to make it so; a pipe or a device, such as `/dev/stderr`, is
written as it is. Each command appends, after its own first line, so a CI job
can set `CLUSTERCTL_PROGRESS_LOG` once for all its commands; lines of two
commands writing at once do not cut into each other on a local file system.
An empty `--progress-log` writes no log, whatever the variable says.

The events are written in batches as they come, and the rest as the command
ends, after Ctrl-C too; a second Ctrl-C ends clusterctl at once, and the last
events are lost with it. A log that cannot be written, on a full disk for
instance, fails nothing: the command ends as it would have, and one line on
standard error says the log stops short. The commands that hand the terminal
to another program report no progress and write nothing, and neither do the
commands an agent runs through `clusterctl mcp`.

When `TRACEPARENT` holds a valid [W3C trace context](https://www.w3.org/TR/trace-context/),
as a CI system that traces its jobs may set it, the command continues that
trace: its events carry the trace's id, and its first line also names the
span it runs under, the trace flags and `TRACESTATE`, as they were given:

```json
{"v":1,"type":"trace","trace":"4bf92f3577b34da6a3ce929d0e0e4736","parent":"00f067aa0ba902b7","traceFlags":"01","traceState":"ci=build-42"}
```

Commands that continue one trace share its id and never a span's. A
`TRACEPARENT` that is not valid is ignored, and whether the trace is sampled
changes nothing: the log is written either way. clusterctl sends no trace
anywhere; the log is what a converter to a tracing system would read.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Everything succeeded |
| 1 | clusterctl worked, at least one target failed |
| 2 | The command line or the configuration was rejected |
| 3 | A host could not be reached or authenticated with |
| 130 | Interrupted, or a confirmation was declined |

The difference between 1 and 3 is what lets a script tell "the node said no"
from "the node was not there":

```bash
clusterctl exec -n '@compute' -- systemctl is-active slurmd
case $? in
  0) echo "all healthy" ;;
  1) echo "some nodes are unhealthy" ;;
  3) echo "some nodes are unreachable" ;;
  *) echo "clusterctl could not run" ; exit 2 ;;
esac
```

Read `$?` straight after clusterctl. After `if ! clusterctl …; then` it holds
the status of the `!`, which is always 0 inside the `then`. Under `set -e`,
keep the status with `rc=0; clusterctl … || rc=$?` and switch on `$rc`.

## Scripting safely

```bash
#!/usr/bin/env bash
set -euo pipefail

export CLUSTERCTL_CONTEXT=cluster1

# -y confirms in advance. Without a terminal and without -y, a destructive
# command refuses rather than proceeding unasked.
clusterctl slurm node drain "maintenance $(date -I)" -n '@rack:R02' -y

# Wait for the jobs to drain.
while [ "$(clusterctl slurm job list -n '@rack:R02' -o json | jq length)" -gt 0 ]; do
  sleep 60
done

clusterctl bmc power soft -n '@rack:R02' -y
```

Put `--dry-run` in front of it first. It prints what would happen and changes
nothing; the lookups it needs, such as the Slurm job check, still run.

## Overriding configuration for one command

```console
$ clusterctl --set fanout.max=4 exec -n '@compute' -- uptime
$ clusterctl --set ssh.connectTimeout=30s login install
$ CLUSTERCTL_FANOUT=4 clusterctl exec -n '@compute' -- uptime
```

`--set` takes a dotted path into the merged configuration and wins over
everything else. `clusterctl config view --show-sources` lists the paths.
