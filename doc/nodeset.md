<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Node sets

A node set expression names a set of hosts, in ClusterShell's syntax.
clusterctl reads it with [go-nodeset](https://github.com/GSI-HPC/go-nodeset),
at the release `go.mod` requires. The
[language reference](https://github.com/GSI-HPC/go-nodeset/blob/v1.0.0/doc/language.md)
of that release describes the syntax, the rules chosen where an
implementation has to choose, every place where it differs from ClusterShell,
and the limits; its
[testing.md](https://github.com/GSI-HPC/go-nodeset/blob/v1.0.0/doc/testing.md)
says how the engine is tested.
[ADR 0026](adr/0026-go-nodeset-and-go-clikit.md) says why the engine is a
module of its own.

This document lists what clusterctl adds to the language. How an expression
becomes nodes, from the group sources to the inventory's names, is
[selection.md](selection.md).

## Where an expression is checked

clusterctl hands go-nodeset no text from outside but through
`internal/nodeexpr`, whose `Parse`, `ParseWith` and `Add` refuse what the two
sections below describe before go-nodeset reads it. That covers:

- the node set a command is asked to select: `-n`, the arguments that name
  nodes, `CLUSTERCTL_NODES` and the node sets of the MCP tools;
- `safety.protectedHosts`;
- the `nodes` of `NodeInventory` documents and of `bootPath` rules;
- the expression every group source answers, the groups it names in turn and
  each group of `@source:*` included;
- the first column that `-o nodeset` and `-o name` read, such as the host
  patterns of the known hosts file that `hostkey list` prints;
- a node name with brackets that the inventory is asked for, as by
  `node rack`;
- and the node names read from Slurm, a drain reason or a node list of
  `squeue`, from the naming templates and the host templates of the service
  processors, and from the output of a remote command.

A node name that `sinfo --Node` prints, checked first to be a single host
name without brackets, is added to a set as it is.

An error in what a group source answered names the group, as in
`group @bad: in "exe[1-]": the range "1-" has no last bound`.

## A range needs its last bound

`exe[1-]`, `exe[1-,5]` and `exe[1-/2]` are refused,
`in "exe[1-]": the range "1-" has no last bound`, as ClusterShell refuses
them; go-nodeset v1.0.0 reads the first two as `exe1` and `exe[1,5]`, and
refuses the third with an error of its own. The hazard is the shell's:
`-n "exe[1-$N]"` with `N` empty or unset arrives as `exe[1-]`, and a command
meant for many nodes would run on one.

## A reference names a group

`@`, `@:` and `@source:`, a reference without a group name, are refused
wherever groups are resolved, `empty group name in @rack:`, and
`internal/groups` refuses an empty name itself as well. go-nodeset hands the
empty name to the resolver, and a source that reads a node attribute would
answer it with every node that carries the attribute: `-n "@rack:$RACK"` with
`RACK` empty would select every node in a rack. An `exec` source would run its
command with an empty `$GROUP`.

## Groups are clusterctl's

The groups are resolved by clusterctl's own sources, not by go-nodeset's
`MapResolver`, and two of their answers differ from that resolver's: a group
no source defines is an error rather than no hosts, and `@source:*` evaluates
each group of a source on its own rather than joining their expressions into
one, which is read left to right ([selection.md](selection.md#groups)).

A bare `@group` inside a group of a named source is looked up in that source,
as go-nodeset and ClusterShell do: with `compute: "@exe"` in the source
`static`, `@static:compute` asks `static` for `exe`. A bare `@group` anywhere
else searches the sources in order, as `@compute` does.

The groups an expression names are looked up side by side before it is
evaluated, `fanout.PerHost` at a time, a level of nesting at a time:
`internal/groups` answers them through `nodeexpr.BatchResolver`, since every
lookup of an `exec` source is a round trip to a host.

## Steps are read but not written

clusterctl never asks go-nodeset for steps, so `exe[1-10/2]` is read and
prints as `exe[1,3,5,7,9]`. go-nodeset's autostep is not used.

## Names with several numbers

go-nodeset folds a set whose names hold several numbers as ClusterShell does.
clusterctl v0.4.0 folded such a set its own way, so the same hosts can print
otherwise than they did: `rack[1-2]node[01-04],rack3node01` prints as it is,
where v0.4.0 printed `rack[1-3]node01,rack[1-2]node[02-04]`. The host list
handed to Slurm and FreeIPMI is the same as before.
