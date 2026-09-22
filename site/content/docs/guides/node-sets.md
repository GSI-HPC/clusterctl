---
title: Selecting nodes
weight: 1
---

Every command that acts on nodes takes a node set. The syntax is ClusterShell's,
so it is what you already type at `nodeset`, `clush` and `sinfo`.

## The syntax

```console
$ clusterctl node select 'exe0001'                 # one host
$ clusterctl node select 'exe[1-10]'               # a range
$ clusterctl node select 'exe[0001-0010]'          # padded
$ clusterctl node select 'exe[1-10/2]'             # with a step
$ clusterctl node select 'exe[1,5,9-12]'           # several ranges
$ clusterctl node select 'rack[1-2]node[01-04]'    # two dimensions
$ clusterctl node select '@compute'                # a group
$ clusterctl node select '@slurm:idle'             # a group from a source
```

## Combining sets

| Operator | Meaning | Example |
| --- | --- | --- |
| `,` or a space | union | `exe[1-4],sub[1-2]` |
| `!` | difference | `@compute!@drained` |
| `&` | intersection | `@idle&@rack:R02` |
| `^` | in exactly one | `@yesterday^@today` |

{{< callout type="warning" >}}
Operators have **no precedence**. An expression is evaluated strictly left to
right, so `@a!@b&@c` is `((@a minus @b) intersect @c)`. Write the order you
mean; there are no brackets for grouping.
{{< /callout >}}

## Folding and expanding

```console
$ clusterctl node select 'exe[1-3],exe7'
exe[0001-0003,0007]

$ clusterctl node select 'exe[1-3]' --expand
exe0001
exe0002
exe0003

$ clusterctl node select '@compute' --count
1024
```

{{< callout type="info" >}}
Note the padding. The inventory wrote these machines as `exe0001`, so a set
comes back under the names the site gave them even when you typed `exe1`.
`exe1` and `exe0001` are the same host: padding is how a name is displayed,
not part of what it identifies.
{{< /callout >}}

## Groups

A group is resolved by a source. Which sources exist is configuration:

```console
$ clusterctl node groups
SOURCE     GROUP
inventory  exe
inventory  sub
inventory  wlm
rack       R01
rack       R02
slurm      main
slurm      debug
```

A bare `@group` searches the default source first and then the others, so
`@exe` works without a prefix. `@source:group` names one explicitly, which is
what to write when two sources could both answer.

```console
$ clusterctl node groups exe0007
SOURCE     GROUPS
inventory  exe
rack       R02
slurm      main
```

### Where groups come from

```yaml
groups:
  defaultSource: inventory
  sources:
    inventory:
      # One group per value of a node attribute. This is what a genders
      # file provided: @exe, @wlm, @dbm.
      attribute: class

    rack:
      attribute: rack

    static:
      # A table. A group may refer to other groups.
      static:
        infra: wlm01,dbm01
        compute: "@inventory:exe"

    slurm:
      # Asked of the workload manager and cached for a minute.
      cacheTtl: 60s
      exec:
        role: login
        map: [sinfo, -h, -o, "%N", -p, $GROUP]
        all: [sinfo, -h, -o, "%N"]
        list: [sinfo, -h, -o, "%R"]
        reverse: [sinfo, -h, -N, -o, "%R", -n, $NODE]
```

An `exec` source is given an argument vector, and `$GROUP` and `$NODE` are
substituted as whole arguments. A group name is never interpreted by a shell.

## A selection for a whole session

```console
$ export CLUSTERCTL_NODES='@rack:R02'
$ clusterctl bmc status
$ clusterctl exec -- uptime
```

`-n` always wins over it. Nothing else falls back to a set you set earlier: a
command with no selection stops rather than guessing.

## Passing a set to another tool

```console
$ clusterctl slurm node nodeset drain
exe[0007,0042,0511]

$ clusterctl exec -n "$(clusterctl slurm node nodeset idle)" -- uptime
```

Slurm and FreeIPMI understand one bracketed range per name and neither several
numeric dimensions nor groups, so clusterctl expands a set for them when it has
to. You never have to think about it.

## Checking a set before you use it

```console
$ clusterctl node select '@rack:R02&@slurm:idle' --expand
$ clusterctl node list '@rack:R02'
$ clusterctl node fqdn -n '@rack:R02'
```

Worth doing before anything destructive. `--dry-run` also prints the set it
resolved before it stops.
