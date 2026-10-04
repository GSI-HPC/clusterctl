<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Node sets

A node set expression names a set of hosts, in ClusterShell's syntax.
clusterctl reads it with [go-nodeset](https://github.com/GSI-HPC/go-nodeset),
at the release `go.mod` requires. The
[language reference](https://github.com/GSI-HPC/go-nodeset/blob/v1.0.1/doc/language.md)
of that release describes the syntax, the rules chosen where an
implementation has to choose, every place where it differs from ClusterShell,
and the limits; its
[testing.md](https://github.com/GSI-HPC/go-nodeset/blob/v1.0.1/doc/testing.md)
says how the engine is tested.
[ADR 0026](adr/0026-go-nodeset-and-go-clikit.md) says why the engine is a
module of its own.

This document lists what clusterctl adds to the language. How an expression
becomes nodes, from the group sources to the inventory's names, is
[selection.md](selection.md).

## Where groups are resolved

clusterctl resolves groups through `internal/groups`, in:

- the node set a command is asked to select: `-n`, the arguments that name
  nodes, `CLUSTERCTL_NODES` and the node sets of the MCP tools;
- `safety.protectedHosts`;
- and the expression every group source answers, the groups it names in turn
  and each group of `@source:*` included.

Everything else clusterctl reads as a node set, such as the `nodes` of
`NodeInventory` documents and of `bootPath` rules, or the node names read from
Slurm, goes to go-nodeset without a resolver, so go-nodeset refuses every
group reference in it.

An error go-nodeset finds in what a group source answered does not name the
group: `in "exe[1-]": the range "1-" has no last bound`. An error of the
resolver does, as in `group @rack:: the group name is empty`.

## A range needs its last bound

`exe[1-]`, `exe[1-,5]` and `exe[1-/2]` are refused,
`in "exe[1-]": the range "1-" has no last bound`, as ClusterShell refuses
them. go-nodeset refuses them itself since v1.0.1; v1.0.0 read the first two
as `exe1` and `exe[1,5]`, and clusterctl refused them in front of it. The
hazard is the shell's: `-n "exe[1-$N]"` with `N` empty or unset arrives as
`exe[1-]`, and a command meant for many nodes would run on one.

## A reference names a group

`@`, `@:` and `@source:`, a reference without a group name, are refused by
`internal/groups` before it asks any source,
`group @rack:: the group name is empty`. go-nodeset hands the empty name to
the resolver, and a source that reads a node attribute would answer it with
every node that carries the attribute: `-n "@rack:$RACK"` with
`RACK` empty would select every node in a rack. An `exec` source would run its
command with an empty `$GROUP`.

## Groups are clusterctl's

The groups are resolved by clusterctl's own sources, not by go-nodeset's
`MapResolver`, and a group no source defines is an error rather than no
hosts, as that resolver answers. `@source:*` evaluates each group of a source
on its own, as that resolver does since go-nodeset v1.0.1, rather than
joining their expressions into one, which is read left to right
([selection.md](selection.md#groups)).

A bare `@group` inside a group of a named source is looked up in that source,
as go-nodeset and ClusterShell do: with `compute: "@exe"` in the source
`static`, `@static:compute` asks `static` for `exe`. A bare `@group` anywhere
else searches the sources in order, as `@compute` does.

The groups an expression names are looked up side by side before it is
evaluated, `fanout.PerHost` at a time, a level of nesting at a time:
`internal/groups` answers them through `nodeexpr.BatchResolver`, since every
lookup of an `exec` source is a round trip to a host. go-nodeset asks for one
group after the other, so `internal/nodeexpr` reads the references of each
level itself first. An expression that fails at one group may so have had
the other groups of its level looked up already; a lookup only reads.

## Steps are read but not written

clusterctl never asks go-nodeset for steps, so `exe[1-10/2]` is read and
prints as `exe[1,3,5,7,9]`. go-nodeset's autostep is not used.

## Names with several numbers

go-nodeset folds a set whose names hold several numbers as ClusterShell does.
clusterctl v0.4.0 folded such a set its own way, so the same hosts can print
otherwise than they did: `rack[1-2]node[01-04],rack3node01` prints as it is,
where v0.4.0 printed `rack[1-3]node01,rack[1-2]node[02-04]`. The host list
handed to Slurm and FreeIPMI is the same as before.
