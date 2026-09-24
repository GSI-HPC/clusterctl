---
title: Exit codes
weight: 3
---

These are part of the command line contract. They may be added to but never
renumbered.

| Code | Meaning | Typical cause |
| --- | --- | --- |
| `0` | Everything succeeded | |
| `1` | clusterctl worked, at least one target failed | A command exited non-zero on a node; a service processor rejected an action |
| `2` | Usage or configuration error | An unknown flag, an unparseable node set, a configuration that does not validate, a protected host |
| `3` | Transport | A host could not be reached, resolved or authenticated with |
| `130` | Interrupted | Ctrl-C, or a confirmation declined |

## Why 1 and 3 are separate

A wrapper needs to tell "the node said no" from "the node was not there".

```bash
clusterctl exec -n '@compute' -- systemctl is-active slurmd
case $? in
  0) echo "all healthy" ;;
  1) echo "some nodes are unhealthy" ;;         # act on it
  3) echo "some nodes are unreachable" ;;       # check the network first
  *) echo "clusterctl could not run" ; exit 2 ;;
esac
```

When a command that works on many hosts sees several of these, it exits with
the first that applies of `130` (a host was not tried because of an
interrupt), `3` (a host could not be reached) and `1` (a host answered with a
failure). A run in which one node refused and another was down exits `3`.

## A declined confirmation is 130

Declining a prompt is an interruption, not a failure:

```console
$ clusterctl bmc power off -n exe0001
About to power off 1 host: exe0001
Continue? [y/N] n
clusterctl: not confirmed, nothing was done
$ echo $?
130
```

A destructive command with no terminal to ask on exits `2`, because that is a
usage problem: pass `-y` if you meant it.

## A dry run is 0

`--dry-run` prints what would happen and exits zero, having sent nothing.
