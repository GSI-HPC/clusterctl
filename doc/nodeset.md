<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Node sets

A node set expression names a set of hosts. The syntax is ClusterShell's,
because that is what the team already types and what the cluster's other tools
accept.

The engine is in `nodeset/`, the one package this module offers to other
programs: when this was written, Go had no node set implementation, and porting
ClusterShell's semantics is the kind of work that should be done once.

## Syntax

```
exe0001                   one host
exe[1-10]                 a range
exe[0001-0010]            a padded range
exe[1-10/2]               a range with a step
exe[1,5,9-12]             several ranges
rack[1-2]node[01-04]      two numeric dimensions
exe[1-4].hpc.example.org  a name with a domain
@compute                  a group
@slurm:idle               a group from a named source
@*                        every host the default source knows
```

Set operators combine expressions:

| Operator | Meaning |
| --- | --- |
| `,` or whitespace | union |
| `!` | difference |
| `&` | intersection |
| `^` | symmetric difference |

Operators have **no precedence**; an expression is evaluated strictly left to
right. `exe[1-10]!exe[1-5]&exe[1-7]` is `((exe[1-10] minus exe[1-5]) intersect
exe[1-7])`, which is `exe[6-7]`. Write the order you mean.

Whitespace unions, so the arguments of a command line can be joined with a
space and parsed in one call.

`!`, `&` and `^` need an operand on each side. `@rack:R02&`, `exe[1-10]!,exe5`,
`exe[1-10]&&exe5` and `!exe5` are errors. The first is what
`-n "@rack:R02&$(clusterctl slurm node nodeset idle)"` leaves behind when no
node is idle, and reading it as `@rack:R02` would select the whole rack instead
of nothing. A union tolerates an empty operand, because a union with nothing is
what was meant: `exe1,` is `exe1`.

## Semantics chosen here

These are the corners where an implementation has to decide something. They are
written down because the behaviour is observable.

### Every run of digits is a dimension

`x1y1` has two numeric dimensions, `10.0.1.7` has four, and `exe0001` has one,
whether or not brackets were written. A dimension holding a single value is
rendered without brackets, so folding is idempotent: the printed form of a set
parses back into the same set, every host spelled as before, and prints the
same way again. This property is checked by a fuzz test.

### Padding is not part of a host's identity

**Decision:** names that differ only in zero padding, such as `exe1`, `exe01`
and `exe0001`, are **one host**. `exe1,exe01` names one host, `exe[1-3]!exe02`
is `exe[1,3]`, and a set holding `exe0001` contains `exe1`.

This is what lets an administrator type `exe1` and reach the machine an
inventory wrote as `exe0001`, and see it under the name the site gave it,
because a selection is canonicalised against the inventory. The price is that
a site cannot have two machines whose names differ only in padding: they would
be one host to every command, so an inventory holding such a pair has to be
rejected rather than one of them picked.

Padding is still never thrown away. Each host keeps the spelling it was first
given, and a set never shows a host under a name it was not given:

- `exe1,exe01` prints `exe1`, and `exe01,exe1` prints `exe01`.
- `exe[01-02]` plus `exe3` prints `exe[01-02,3]`, not `exe[01-03]`.
- A set holding `exe[0001-0010]` and `exe11` prints `exe[0001-0010,11]`, and
  `Canonical("exe11")` answers `exe11`.
- A value only joins a range when it reads the same at the range's width, so
  `exe08,exe09,exe10` prints `exe[08-10]` but `exe7,exe08,exe9` prints
  `exe[7,08,9]`.

In a range the width of the first bound applies to the whole range:
`exe[01-100]` is `exe01` to `exe99` and `exe100`. A last bound padded to
another width, as in `exe[1-010]` or `exe[001-10]`, is an error, because one of
the two bounds would be shown under a name it was not written as.

### Adjacent numeric parts are rejected

`exe0[0,10]` is an error. It expands to `exe00` and `exe010`, and neither name
can be split back into the same two dimensions, so folding it would silently
lose a host. Separate numeric parts with a literal character.

### Steps are read but not written

`exe[1-10/2]` parses. Folding does not produce a step unless it is asked for,
which is what ClusterShell's `--autostep` does; without it, `exe[1,3,5,7]`
prints as it is.

## Groups

A `@group` reference is resolved by a source. Three kinds exist:

```yaml
groups:
  defaultSource: inventory
  sources:
    inventory:
      # One group per value of a node attribute: @exe, @wlm, @dbm.
      # This is what the genders file provided.
      attribute: class
    static:
      # A table written in the configuration. A group may name others.
      static:
        infra: wlm01,dbm01
        compute: "@inventory:exe"
    slurm:
      # Asked of the workload manager, cached for a minute.
      cacheTtl: 60s
      exec:
        role: login
        map: [sinfo, -h, -o, "%N", -p, $GROUP]
        all: [sinfo, -h, -o, "%N"]
        list: [sinfo, -h, -o, "%R"]
        reverse: [sinfo, -h, -N, -o, "%R", -n, $NODE]
```

A bare `@group` searches the default source first and then the others, so a
single-source installation never needs a prefix. `@source:group` names one.

An `exec` source is given an **argument vector**, not a command line, and
`$GROUP` and `$NODE` are substituted as whole arguments. A group name holding a
semicolon stays a group name.

Groups may refer to groups. A cycle is reported after sixteen levels rather
than looping.

### Names are host names

The node set language accepts more than a host name may contain, because a
set is also used for things that are not hosts. A node name, though, becomes
an ssh destination and the host of a Redfish URL, where a leading `-` is an
option and `:`, `@`, `/`, `?` and `#` set the port, the account, the path, the
query and the fragment. So `App.Select`, which every command and the MCP
server use to turn an expression into nodes, refuses a selection with any name
that is not a host name: dot separated labels of ASCII letters, digits and
hyphens, none empty, none longer than 63 characters, none beginning or ending
with a hyphen, and at most 253 characters in all, with an optional final dot.
The check runs on every name the expression resolves to, so names from a group
source or the inventory are held to it too. It lives in `internal/hostname`,
and ssh and the Redfish client apply it again to the host they are given.

## Rendering for other tools

`clusterctl node select` prints the folded form. Anything handed to Slurm or
FreeIPMI goes through `NodeSet.Hostlist()` instead, which folds each pattern
along the one dimension that gives the fewest names, the last one on a tie, and
never writes a step. `rack[1-2]node[001-100]` becomes
`rack1node[001-100],rack2node[001-100]`, and a BMC name such as
`exe[0001-4600].mgmt.dc2.example.org` stays one name rather than 4,600.

Every name then carries at most one bracketed range. That is the form every
version of the two host list parsers reads. `scontrol` 23.11 and `ipmipower`
1.6.13 also read several ranges in one name, but older ones are not known to,
and the one-range form is short enough: it grows with the number of values of
the other dimensions, not with the number of hosts. Neither reads `@groups`,
which are resolved before a set is rendered.

## Limits

A bracket is capped at 2²⁰ elements, counting all of its parts together, and
an expression at 2²⁰ hosts. The expression cap holds at every step of the
evaluation, so `a[1-600000],b[1-600000]!b[1-600000]` is refused even though it
ends up smaller. It also holds for all the terms of an expression together,
groups and the groups they refer to included, so the work an expression costs
is bounded as well as its result: `a[1-600000]!a[1-600000],b1` is refused too.

Each dimension of a name is weighed before it is expanded, against what the
dimensions before it and the terms before it have left, so an oversized
expression is refused before its memory is spent. A typo such as
`exe[1-100000000]` is reported rather than exhausting memory. The most an
expression can cost is the largest set it may name, about 260 MiB of
allocation and a second of time for 2²⁰ hosts.

Bounds and steps are plain decimal numbers of at most eighteen digits, so no
arithmetic on them can overflow.

Folding costs O(n log n) in the number of hosts. Parsing and printing a set of
a million hosts, the most an expression may name, takes a few seconds.
