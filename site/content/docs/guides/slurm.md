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

State groups: `alloc`, `idle`, `drain`, `down`, `defect`. A group means the
state Slurm reports for the node: `idle` leaves out a drained node, although
Slurm counts its base state as idle, and `down` leaves out a node that only
stopped responding or waits to be powered down. Anything else is passed to
Slurm as it is written, so `--state mixed,completing` works too.

A node without a reason shows an empty `REASON`, although `sinfo` prints
`none` for it.

## Taking a node out of production

```console
$ clusterctl slurm node drain 'ticket 4711: failing DIMM' -n exe0007
About to drain 1 host: exe0007
  reason: "ticket 4711: failing DIMM"
Continue? [y/N] y
drained exe0007
```

{{< callout type="warning" >}}
The reason is the first argument, not an option, and it is mandatory. A drained
node with no reason is a node nobody dares resume. Say what is wrong and where
it is tracked.
{{< /callout >}}

The reason is checked before anything is shown, and a bad one exits `2`: it
may not be blank, longer than 200 characters, hold a line break or another
control character, or hold `|`, which Slurm's parsable output puts between
fields. A reason that reads as nodes, such as `exe0007` or `@rack:R02`, is
refused, because that is what a forgotten reason looks like. Name the nodes
either after the reason or with `-n`, not both.

```console
$ clusterctl slurm node resume -n exe0007
```

Before drain or resume shows anything, the nodes are checked against Slurm.
`scontrol` does not read its node list as plain host names: `ALL` means every
node, and a `NodeSet` name from `slurm.conf` means its members. So `ALL`, in any
case, is refused, and so is a set that Slurm does not read as exactly the
nodes named, or that holds a node Slurm does not know:

```console
$ clusterctl slurm node drain 'rack maintenance' -n gpunodes
clusterctl: Slurm reads gpunodes as more nodes than were named, among them gpu[01-08]; name the nodes themselves
```

This check reads the cluster even under `--dry-run`, which still changes
nothing. `scontrol update` is not atomic: when it fails part way, Slurm's own
message is shown, and the nodes are read back to say which of them changed.

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

The window is sent as `now-SECONDS`, so it is counted on the clock of the
login node and does not move with the time zone of your workstation.

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

Changes to the accounting database name the cluster, read from the
`ClusterName` of `slurm.conf` on the login node, because one `slurmdbd` can serve
several clusters and `sacctmgr` applies a change that names none to all of
them. The confirmation says which cluster. Account and user names are letters,
digits and `. _ @ -` only: `sacctmgr` reads a comma as a list and brackets as a
range, so `proj[1-100]` would create a hundred accounts.

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
About to create the Slurm user alice with the account proj in the Slurm cluster hpc on 1 host: accounting
Continue? [y/N] y
user alice associated

$ clusterctl slurm user add alice proj proj
$ clusterctl slurm user default alice proj
```

`ACCOUNT` defaults to `slurm.defaultAccount`. `DEFAULT_ACCOUNT` becomes the
user's default account, for a user who exists already too, and a default
account the user is not associated with is associated as well. The
confirmation says what the defaults resolved to.

{{< callout type="info" >}}
Before a user is added, the login node is asked whether it knows that account
at all, with `getent passwd`. `getent` reads the name service switch, local
files included, so a local account on the login node passes this check;
check the directory yourself when that matters. A number is refused, because
`getent` would read it as a user ID. The check runs under `--dry-run` too.
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
