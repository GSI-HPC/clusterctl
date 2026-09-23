<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0010 — Publish `nodeset`, keep the rest internal

Status: accepted

## Context

Go's `internal/` keeps a package private to its module. The default for an
application is to put everything there: nothing is exported, so nothing has to
stay stable.

## Decision

Everything is under `internal/` except `nodeset`, which sits at the module
root and is offered as a library.

## Why

The Go ecosystem has no node set implementation. That is a stated finding of
the review, and it is why this one had to be written
([0002](0002-own-nodeset-engine.md)). Keeping it internal would mean the next
program that needs one writes a third.

The package is also the right shape to publish: it has no dependencies beyond
the standard library, it has one concept, its semantics are documented and
fuzz-tested, and it does not need configuration, a transport or a filesystem.

The rest is not. `internal/config` is about clusterctl's five document kinds,
`internal/transport` is about how this program wants to reach a host, and
`internal/app` exists to wire this program together. Exporting them would
freeze decisions that should stay changeable.

## What that commits to

`nodeset` follows the module's semantic version. A change to its exported
surface, or to the folded form it produces, is a major version of clusterctl.

The folded form counts as interface, not implementation: programs will compare
its output, and a set that renders differently after an upgrade breaks them.

## Costs

- The package cannot borrow from `internal/`. It does not need to today; if it
  ever does, that is a sign the borrowed thing belongs in it or does not belong
  in the package at all.
- Someone importing it is a user whose upgrade this project has to consider.
  That is the point.
