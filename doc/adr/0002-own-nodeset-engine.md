<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0002 — Port the node set engine rather than depend on one

Status: accepted

## Context

Node sets are the tool's central noun. Every command takes one, and the team
already types ClusterShell syntax at `nodeset`, `clush`, `sinfo` and
`scontrol`.

Go has no node set implementation. Rust has one crate, which matched
ClusterShell on 46 of 50 test expressions and has a single maintainer. Python
has ClusterShell itself.

## Decision

Implement the engine here, in `nodeset/` at the module root.

## Why

- There is nothing to depend on. The choice is between writing it and not
  having node sets, which is not a choice.
- The semantics have to be exactly right, because a set that is subtly wrong
  powers off the wrong machine. Owning the code means owning the corner cases
  rather than reporting them upstream and waiting.
- The engine is useful beyond this program, which is why it is the one package
  outside `internal/`. See [0010](0010-public-nodeset-package.md).

## What was decided about the semantics

Three things an implementation has to pick, picked the way ClusterShell picks
them and written down in [../nodeset.md](../nodeset.md):

- Every run of digits is a dimension, and a dimension with one value renders
  without brackets. This makes folding idempotent, which is checked by a fuzz
  test.
- Padding is a display property of a dimension, not part of a host's identity:
  `exe1` and `exe01` are one host. This is why a selection is canonicalised
  against the inventory, so typing `exe1` reaches the machine written down as
  `exe0001`.
- A name whose numeric parts are adjacent, such as `exe0[0,10]`, is rejected.
  It expands to names that cannot be split back the same way, so folding would
  lose a host silently. The fuzz test found this.

## Costs

- Roughly 800 lines to maintain, plus the semantics to keep aligned with
  ClusterShell as it changes.
- Divergence from ClusterShell is possible and would be found by a site rather
  than by a test. Running the two against each other over a corpus of real
  expressions would close that gap and has not been done.
