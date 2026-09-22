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
| `jsonpath=…` | A field or a template. |
| `jq=…` | A jq program, embedded; no jq binary needed. |

```console
$ clusterctl slurm node list --state drain -o nodeset
exe[0007,0042,0511]

$ clusterctl node list -o json | jq '.[] | select(.rack == "R02") | .name'

$ clusterctl bmc status -n '@rack:R02' -o jsonpath='{.[*].state}'
On On Off On

$ clusterctl exec -n '@compute' -o jq='.[] | select(.exitCode != 0) | .target.name' -- true
```

{{< callout type="info" >}}
Table output is never truncated. A long drain reason wraps rather than being
cut off, because the important half of a message is usually the end of it. The
last column is not padded, so a copied line carries no trailing spaces.
{{< /callout >}}

## Progress and errors go to stderr

Everything a program would parse goes to standard output; notes, progress and
prompts go to standard error. Redirecting one does not lose the other.

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
if ! clusterctl exec -n '@compute' -- systemctl is-active slurmd; then
  case $? in
    1) echo "some nodes are unhealthy" ;;
    3) echo "some nodes are unreachable" ;;
    *) echo "clusterctl could not run" ; exit 2 ;;
  esac
fi
```

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

Put `--dry-run` in front of it first. It prints what would happen and exits
zero, having contacted nothing.

## Overriding configuration for one command

```console
$ clusterctl --set fanout.max=4 exec -n '@compute' -- uptime
$ clusterctl --set ssh.connectTimeout=30s login install
$ CLUSTERCTL_FANOUT=4 clusterctl exec -n '@compute' -- uptime
```

`--set` takes a dotted path into the merged configuration and wins over
everything else. `clusterctl config view --show-sources` lists the paths.
