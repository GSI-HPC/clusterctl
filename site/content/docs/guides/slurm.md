---
title: Slurm
weight: 5
---

The Slurm clients run on a host the configuration names. clusterctl asks them
for their parsable output with an explicit field list and splits on the
separator — never for the aligned display form, which moves with the terminal
width and truncates.

## Nodes

```console
$ clusterctl slurm node list
NODE     STATE    PARTITION  CPUS  REASON
exe0001  idle     main        128
exe0007  drained  main        128  ticket 4711: failing DIMM [alice]

$ clusterctl slurm node list --state defect
$ clusterctl slurm node list --state idle -o wide
$ clusterctl slurm node list '@rack:R02'
```

State groups: `alloc`, `idle`, `drain`, `down`, `defect`. Anything else is
passed to Slurm as it is written, so `--state mixed,completing` works too.

## Taking a node out of production

```console
$ clusterctl slurm node drain 'ticket 4711: failing DIMM' -n exe0007
About to drain 1 host: exe0007
  reason: ticket 4711: failing DIMM
Continue? [y/N] y
drained exe0007
```

{{< callout type="warning" >}}
The reason is the first argument, not an option, and it is mandatory. A drained
node with no reason is a node nobody dares resume. Say what is wrong and where
it is tracked.
{{< /callout >}}

```console
$ clusterctl slurm node resume -n exe0007
```

## Node sets by state

```console
$ clusterctl slurm node nodeset idle
exe[0001-0006,0008-1024]

$ clusterctl exec -n "$(clusterctl slurm node nodeset drain)" -- dmesg -T | tail
```

When no node is drained, the inner command prints nothing and the `-n` it fills
in is refused with exit code 2; `CLUSTERCTL_NODES` is not used in its place.

## The queue

```console
$ clusterctl slurm job list --state running
JOB    USER   ACCOUNT  PARTITION  STATE    NODES          RUNTIME
4711   alice  proj     main       RUNNING  exe[0001-0004]  10:32:11

$ clusterctl slurm job list -n exe0007
$ clusterctl slurm job list --state pending -o wide
$ clusterctl slurm job summary
USER   ACCOUNT  PARTITION  JOBS
alice  proj     main         42
bob    proj     main          7
```

## What finished

```console
$ clusterctl slurm job history --state failed --since 24h
JOB   USER   ACCOUNT  STATE   EXIT  ELAPSED
4699  alice  proj     FAILED  1:0   00:05:12

$ clusterctl slurm job history --jobs 4711,4712
$ clusterctl slurm job history --user alice --since 7d
```

## Partitions

```console
$ clusterctl slurm partition
PARTITION  AVAIL  NODES  MAX TIME
main       up      1020  7-00:00:00
debug      up         4  02:00:00

$ clusterctl slurm partition main -o wide
```

## Accounts

```console
$ clusterctl slurm account list
ACCOUNT  DESCRIPTION  COORDINATORS
proj     Project      alice,bob

$ clusterctl slurm account add newproj
$ clusterctl slurm account coordinator newproj alice bob
$ clusterctl slurm account limits proj
$ clusterctl slurm account shares proj
$ clusterctl slurm account shares proj 100
```

## Users

```console
$ clusterctl slurm user list alice
USER   ACCOUNT  DEFAULT  FAIRSHARE
alice  proj     proj     1

$ clusterctl slurm user add alice proj
$ clusterctl slurm user default alice proj
```

{{< callout type="info" >}}
Before a user is added, the cluster is asked whether it knows that account at
all — with `getent`, not `id`, so a local account on the login node is not
mistaken for a directory user.
{{< /callout >}}

```console
$ clusterctl slurm user add ghost proj
clusterctl: the cluster does not know a user account "ghost"; check the directory before adding it to Slurm
```

## Power and jobs

`clusterctl bmc power off` refuses a node running a job. The usual sequence is:

```console
$ clusterctl slurm node drain 'ticket 4711: memory replacement' -n exe0007
$ clusterctl slurm job list -n exe0007          # wait for it to empty
$ clusterctl bmc power soft -n exe0007
```

and afterwards:

```console
$ clusterctl bmc power on -n exe0007
$ clusterctl slurm node resume -n exe0007
```
