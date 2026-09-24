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

`!`, `&` and `^` need something on both sides. When a command substitution
prints nothing, `-n "@rack:R02&$(clusterctl slurm node nodeset idle)"` becomes
`@rack:R02&`, which is an error rather than the whole rack:

```console
$ clusterctl node select '@rack:R02&'
clusterctl: in "@rack:R02&": the & operator has no right operand
```

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
`exe1` and `exe0001` are the same host: padding is not part of what a name
identifies. Every host keeps the name the inventory gave it, so a stray `exe11`
next to `exe[0001-0010]` prints as `exe[0001-0010,11]`, never as `exe0011`.
ClusterShell would treat `exe1` and `exe0001` as two hosts.
{{< /callout >}}

## What a name may contain

A node name is used as a host name, so every name a set resolves to has to be
one: letters, digits and hyphens, in labels separated by dots, with no label
beginning or ending with a hyphen. Anything else, such as a name beginning
with `-` or containing `:`, `@`, `/`, `?`, `#`, `_` or a space, is refused
with exit code 2 before anything is sent. This holds for names that come from
a group source or the inventory as well as for names you type, and it is what
stops a name from turning into an ssh option or sending a Redfish request,
with the BMC password, to another host.

```console
$ clusterctl node hw -n '-oProxyCommand=...'
clusterctl: node "-oProxyCommand=..." is not a host name: the label "-oProxyCommand=" begins with a hyphen
```

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

`-n` or a node set argument always wins over it. Nothing else falls back to a
set you set earlier: a command with no selection stops rather than guessing.

Give a node set once. `-n` together with a node set argument, or `-n` twice, is
refused with exit code 2 rather than one of them being ignored; write the union
into one expression instead, such as `-n exe0001,exe0002`.

## Passing a set to another tool

```console
$ clusterctl slurm node nodeset drain
exe[0007,0042,0511]

$ clusterctl exec -n "$(clusterctl slurm node nodeset idle)" -- uptime
```

When no node is in that state, the inner command prints nothing and `-n` is
given an empty set. That is refused with exit code 2, even with
`CLUSTERCTL_NODES` set: an explicit `-n` is never replaced by the session set.

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
