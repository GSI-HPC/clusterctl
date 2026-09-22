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

## Semantics chosen here

These are the corners where an implementation has to decide something. They are
decided the way ClusterShell decides them, and written down because the
behaviour is observable.

### Every run of digits is a dimension

`x1y1` has two numeric dimensions, `10.0.1.7` has four, and `exe0001` has one,
whether or not brackets were written. A dimension holding a single value is
rendered without brackets, so folding is idempotent: the printed form of a set
parses back into the same set and prints the same way again. This property is
checked by a fuzz test.

### Padding is a display property

`exe1` and `exe01` name the **same host**, shown with a width of one or two
digits. A set remembers the first non-zero width it was given for a dimension
and shows every member with it, so `exe[01-02]` plus `exe3` prints
`exe[01-03]`.

This matters beyond printing. An administrator who types `exe1` reaches the
machine an inventory wrote as `exe0001`, and sees it under the name the site
gave it, because a selection is canonicalised against the inventory.

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

## Rendering for other tools

`clusterctl node select` prints the folded form. Slurm and FreeIPMI parse one
bracketed range per name and understand neither several numeric dimensions nor
`@groups`, so anything handed to them goes through `NodeSet.Hostlist()`, which
folds a one-dimensional set and expands anything else.

## Limits

A single range is capped at 2²⁰ elements and a whole set at 2²⁰ hosts. A typo
such as `exe[1-100000000]` is reported rather than exhausting memory.

Folding costs O(n log n) in the number of hosts. Parsing and printing a set of
a million hosts takes a few seconds.
