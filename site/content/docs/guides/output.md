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

An error message often quotes what a node, a BMC or a group source said, so it
is escaped the same way before it is printed, except that newlines and tabs are
kept: some messages are several lines on purpose, such as the list of problems
clusterctl found in a configuration file.

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
