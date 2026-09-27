<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Testing the node set engine

The `nodeset` package is tested on its own, with the standard library alone:
table-driven unit tests, a fuzz target, a differential corpus recorded with
ClusterShell, and tests of size and cost. [nodeset.md](nodeset.md) describes
the language they hold the package to.

## What is tested where

**Unit tests** cover parsing, folding and expansion, the set operators and
their order, groups through a table resolver, host lists, splitting, the
limits, and the spelling a set keeps for a host written with other padding.
The case name says which property is checked.

**A fuzz target**, `FuzzParseFold`, checks the two properties a node set
expression must satisfy: parsing never panics, and folding is idempotent. It
compares the hosts before and after folding name by name, so a fold that
renamed `exe3` to `exe03` would be caught, which a padding-blind membership
check would not. It also checks that `Hostlist` names the same hosts. It is
how the adjacent-numeric-parts ambiguity was found. A fuzzing worker gives up
on any input that runs for ten seconds, so the target lowers the expansion
limits to 2¹² and skips inputs longer than a kilobyte: every input stays
cheap, and the time goes into variety. CI runs it for a bounded time on every
change; locally, run it from the package's directory:

```console
$ go test -run '^$' -fuzz FuzzParseFold -fuzztime 60s .
```

**A differential corpus** holds node set expressions with the answer
ClusterShell gave for each, in `testdata/clustershell.txt`. A test checks that
the package names the same hosts, except on the lines marked as one of the
divergences [nodeset.md](nodeset.md#where-this-differs-from-clustershell)
lists, where it checks that the answers still differ, so that a divergence
which goes away is noticed and taken out of the documentation too.
`clustershell.py` next to it records the answers again, with ClusterShell
installed; the version it used is in the corpus header.

**Size and cost** are tests of their own. One folds sets of a quarter of a
million hosts, which would take more than a minute if folding were quadratic.
One lowers the limits to a hundred and checks that every way of exceeding them
is refused, a range, a pattern, several terms together and groups included.
One measures what two oversized expressions allocate before they are refused,
and holds it to what the largest permitted set costs. Tests that lower the
limits or measure allocation change or read state of the whole package, so
they do not run in parallel.

## Coverage

Every statement of the package is covered. A branch no test can reach is
deleted rather than excluded, and a test asserts on behaviour that would be
wrong if it changed, not on the shape of the implementation.
