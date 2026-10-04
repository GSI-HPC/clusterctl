<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0026 — Node sets from go-nodeset, progress and pools from go-clikit

Status: accepted

It supersedes [0010](0010-public-nodeset-package.md), and part of four
others: of [0002](0002-own-nodeset-engine.md), the decision that the engine
lives in `nodeset/` at the module root and the costs of keeping it and its
corpus here; of [0012](0012-dependabot.md), that every minor and patch
release of a module in `go.mod` comes in the one grouped pull request; of
[0021](0021-progress-as-our-own-events.md), that progress is reported in
`internal/progress`, and the cost that the event model, the displays, the
sanitiser and its fuzz target are clusterctl's to maintain; and of
[0022](0022-bounded-pools-and-power-batches.md), that the pools live in
`internal/clikit/fanout`.

## Context

The node set engine, `nodeset`, was the one package clusterctl offered to
other programs ([0010](0010-public-nodeset-package.md)). The runtime that
reports progress and draws it, `internal/progress` and its `display`, the
escaper under it, `internal/termtext`, and the pools that report their items,
`internal/clikit/fanout`, were written to know nothing of clusterctl, so that
they could move into a module of their own by a change of path. sind, which
runs Slurm clusters in Docker, set out to report its progress the same way,
from a library that other programs import too.

Both moved, with their history, into modules of GSI-HPC:
[go-nodeset](https://github.com/GSI-HPC/go-nodeset), released as v1.0.0 and
required here at v1.0.1, and
[go-clikit](https://github.com/GSI-HPC/go-clikit), released as v0.1.0 and
required here at v0.2.0, which requires go-nodeset v1.0.0 and
`golang.org/x/text` and nothing else. Both are licensed under Apache-2.0,
which GSI, as the copyright holder of the code
([0016](0016-copyright-holder.md)), grants for its copy. Both release from
signed tags, verified as clusterctl's are
([0023](0023-ssh-or-openpgp-release-tags.md)), and the notes of a release are
its tag message. The API of go-nodeset v1.0.0 is that of `nodeset` at
clusterctl v0.4.0; go-clikit's was reviewed before its release and reshaped,
as its notes list.

The packages went on changing here after v0.4.0, and the releases do not
carry every change:

| Commit | In clusterctl | In the release |
| --- | --- | --- |
| `7fd3c03` | `exe[1-]` and `exe[1-,5]` are refused: the range has no last bound (#96) | in go-nodeset v1.0.1; v1.0.0 reads them as `exe1` and `exe[1,5]`, as clusterctl v0.4.0 did |
| `36b2321` | `@source:*` of a source without an `all` command evaluates each group on its own, in `MapResolver` and in `internal/groups` | in go-nodeset v1.0.1's `MapResolver`, not v1.0.0's; `internal/groups` keeps its own, with the two further fixes of v1.0.1 |
| `a32487d` | a pair at the end of a set is folded with autostep 2 | go-nodeset folds steps as ClusterShell does; clusterctl asks for none |
| `a41301e` | the failures of a pool are collected in linear time | in go-clikit |
| `8772c92` | `nodeset.Batch` and `BatchResolver`: the groups an expression names are looked up side by side | not in go-nodeset |
| `e9ece56`, `a110101`, `29d9b45`, `3e1c176`, `ac403ee` | the tree's work for each event and each frame is cut for steps of tens of thousands of targets | in go-clikit v0.2.0, not v0.1.0 |

go-nodeset, at v1.0.0 as at v1.0.1, also differs from clusterctl's parser,
at v0.4.0 as at `c781f6e`, in one place no commit here changed: that parser
refused `@` and `@source:`, a reference without a group name, before it asked
the resolver, and go-nodeset hands the empty name to the resolver.
clusterctl's sources that read a node attribute answer an empty name with
every node that carries the attribute.

## Decision

clusterctl takes its node sets from go-nodeset, and its progress, displays,
escaping and pools from go-clikit, at the releases `go.mod` requires, and
keeps no copy of either.

- **Every package of clusterctl is under `internal/`.** clusterctl offers no
  package to other programs. A program that imported
  `github.com/GSI-HPC/clusterctl/nodeset` imports
  `github.com/GSI-HPC/go-nodeset` instead.
- **The packages map one to one.** `nodeset` is go-nodeset;
  `internal/progress`, `internal/progress/display` and
  `internal/progress/progresstest` are go-clikit's `progress`,
  `progress/display` and `progress/progresstest`; `internal/termtext` is
  `termtext`, whose `EscapeCell` and `EscapeText` are `Escape` and
  `EscapeLines`; and `internal/clikit/fanout` is `fanout`.
- **What is clusterctl's stays here.** `internal/fanout` holds the
  executor that runs a request on many hosts, the status and grouping of its
  results, the bound on each host, and the `Map` that gives a pool
  clusterctl's name, the class of an error by its exit code and
  `fanout.Summarize` ([0020](0020-one-exit-code-rule-for-many-hosts.md)).
  It passes nothing of go-clikit's on: the options, `Each`, `Batches` and
  the types are taken from go-clikit's `fanout`, imported as `pool`.
  `exitcode.Class` is every Bus's `BusOptions.Classify`; `internal/groups`
  resolves the groups, `@source:*` included; and `cli` chooses the display
  and writes the event log.
- **Groups are resolved by `internal/groups`**, through
  `nodeexpr.ParseWith`, in the node set a command is asked to select, from
  `-n`, its arguments, `CLUSTERCTL_NODES` or an MCP tool; in
  `safety.protectedHosts`; and in the expression every group source answers,
  the groups it names and each group of `@source:*` included. What
  clusterctl parses without a resolver, such as the nodes of
  `NodeInventory` documents, goes to go-nodeset as it is, which refuses
  every group reference in it.
- **A range without its last bound stays refused**, by go-nodeset since
  v1.0.1: `exe[1-]`, `exe[1-,5]` and `exe[1-/2]` are refused as they were,
  with `in "exe[1-]": the range "1-" has no last bound`.
- **A reference without a group name is refused** by `internal/groups`,
  before it asks any source: `group @rack:: the group name is empty`.
  Without it, `-n "@rack:$RACK"` with `RACK` empty would select every node
  in a rack, and an `exec` source would run its command with an empty
  `$GROUP`. go-nodeset's own errors in a group's expression do not name the
  group, as clusterctl's parser's did not.
- **The groups of an expression are still looked up side by side.**
  `nodeexpr.Batch`, `BatchResolver` and `Prefetch` are the side-by-side
  lookup of `8772c92`; `internal/groups`' `Resolver` implements
  `BatchResolver`, `fanout.PerHost` at a time, and `nodeexpr.ParseWith`
  prefetches each level of nesting. It is all that `internal/nodeexpr`
  holds.
- **The libraries' tests stay with them.** The fuzz targets of the node set
  parser, the sanitiser and the escaper, the ClusterShell corpus and the
  comparison with ClusterShell itself run in the CI of go-nodeset and
  go-clikit. clusterctl fuzzes its side-by-side lookup,
  `nodeexpr.FuzzParseWith`, against go-nodeset's one after the other, and
  its tests hold every command's events to `progresstest.Check`, as before.
- **Dependabot proposes each of the two modules in a pull request of its
  own**, outside the group of the other modules. A release of either can
  change what clusterctl prints, a minor release of go-clikit, at v0, can
  change its API, and the documents link the release `go.mod` requires, so
  each update is read against its notes and moves those links
  ([release.md](../release.md)).

## Why

- The engine and the runtime were written to be shared, and sind needs the
  runtime. Two copies drift, as these did within days of being copied, and
  every fix would have to be made twice.
- A module of its own gives each a version that says what it promises.
  go-nodeset v1.0.0 keeps its API, the hosts an expression names and what
  `String`, `Hostlist` and `Expand` print through every minor release
  ([its decision 10](https://github.com/GSI-HPC/go-nodeset/blob/v1.0.0/doc/decisions.md#10-the-first-release-is-v100)):
  the promise 0010 made for `nodeset`, kept by a module that does nothing
  else.
- go-nodeset is compared in its CI with ClusterShell 1.10.1 itself, over
  generated expressions and its corpus, and prints what ClusterShell prints
  for names with several numbers, which clusterctl's copy did not.
- A program can import the engine under Apache-2.0 without the Lesser GPL's
  terms for a program that links it, which is what
  [0008](0008-lgpl.md) chose the Lesser variant for. clusterctl itself stays
  LGPL-3.0-or-later.
- The code stays GSI's and first-party, so the reasons of 0002 and 0021 for
  owning it hold: a corner case is fixed where it is found, and no
  third-party module enters the build.
- The refusal of a reference without a group name is kept because a
  selection that names too many nodes is the hazard 0002 owned the engine
  for. It is the resolver's, since what a group means is the resolver's to
  decide, and every expression clusterctl resolves groups in reaches
  `internal/groups`. The refusal of a range without its last bound was kept
  in `internal/nodeexpr` until go-nodeset v1.0.1 refused it.

## Costs

- **What clusterctl prints changes where a name holds several numbers.**
  go-nodeset folds such a set as ClusterShell does, so
  `rack[1-2]node[01-04],rack3node01`, which v0.4.0 printed as
  `rack[1-3]node01,rack[1-2]node[02-04]`, prints as it is, and
  `exe[1-4].dc[1-2].example.org,exe5.dc1.example.org` is no longer
  `exe[1-5].dc1.example.org,exe[1-4].dc2.example.org`. The hosts are the same,
  and so is the host list handed to Slurm and FreeIPMI; a script that compares
  the folded text does not match. `Split` cuts such a set in another place, so
  the batches of a power-on of such names hold other nodes.
- **A bare `@group` inside a group of a named source is looked up in that
  source**, as ClusterShell does: with `compute: "@exe"` in the source
  `static`, `@static:compute` asks `static` for `exe` and fails, where it
  searched every source before. A table that names the source,
  `@inventory:exe`, reads as it did.
- **The displays and the event log change** as go-clikit's release notes
  list. Among what a user sees: a target's name is read as `{}` in an error
  only where it stands alone; the summary counts the nodes of a batch left
  out once; a time from 9.95 s reads `10s`; a pool that a deadline ends
  reports its items canceled, as an interrupt does; `Width` counts an emoji
  with U+FE0F, the soft hyphen and characters made wide after Unicode 15.0 as
  terminals draw them, so a table holding them is laid out wider;
  `dns aliases` names the targets of its steps "resolve the aliases" and
  "read the reverse entries" by the alias and the address, which it left
  unnamed before (`target : ok`, `1 of 2 failed: `), because go-clikit names
  a target whose `Describe` gives no node as `fmt.Sprint` prints the item, a
  change its notes do not list; and the event log's first line names
  `"program":"clusterctl"`, a key version 1 allows.
- **The empty group name is clusterctl's to refuse**, since what a group
  means is the resolver's to decide, and go-nodeset hands it on.
- **The side-by-side lookup reads references outside go-nodeset.**
  `internal/nodeexpr` splits each level into its references as go-nodeset
  splits its terms, which a change to go-nodeset's syntax has to be followed
  by; `FuzzParseWith` holds the two to the same answers. An expression that
  fails at one group may have had the other groups of its level looked up
  already.
- **`internal/groups` keeps its own `@source:*`.** It does not use
  `MapResolver.All`, so a fix to how that joins the groups of a source is
  ported by hand, as the two of go-nodeset v1.0.1 were.
- **A fix to the runtime is a release of another repository first.** A bug
  that shows in clusterctl's tree is fixed in go-clikit, released and then
  taken here; until then clusterctl works around it in its own code, or
  waits.
- **go-clikit is at v0.** A minor release may change its API, and its notes
  say how, so an update of it can be a change to clusterctl's code rather
  than a bump of `go.mod`.
- **The language is documented elsewhere.** `doc/nodeset.md` says only what
  clusterctl adds and links go-nodeset's reference at the release `go.mod`
  requires; each new release moves that link.
- Some 7,500 lines of code and 8,000 of tests left the module
  (`nodeset`, `internal/progress`, `internal/termtext` and
  `internal/clikit` at `c781f6e`), and clusterctl's coverage no longer counts
  them.

## Reconsider when

- go-nodeset looks up the groups of an expression side by side: then
  `internal/nodeexpr` goes;
- or go-clikit reaches v1, and its updates can travel with the others.
