<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Testing

`make test` runs everything. `make cover` reports coverage, `make race` runs
under the race detector, and `make lint` vets and checks formatting.

## What is tested where

**Unit tests** cover the packages that hold the logic: node set parsing and
folding, configuration merging and validation, naming, the inventory, quoting,
output formatting, the DHCP parser, the host key store and the safety gate.

**A real shell** checks the quoting. `shellquote` is the one place where being
subtly wrong is invisible until it eats a production command, so every vector
in the test is quoted, run through `sh -c 'printf %s\n ...'`, and compared with
what went in.

**A fuzz target** checks the two properties a node set expression must satisfy:
parsing never panics, and folding is idempotent. It compares the hosts before
and after folding name by name, so a fold that renamed `exe3` to `exe03` would
be caught, which a padding-blind membership check would not. It runs in CI for a bounded
time and locally with `go test -fuzz`. It is how the adjacent-numeric-parts
ambiguity was found. A fuzzing worker gives up on any input that runs for ten
seconds, so the target lowers the expansion limits to 2¹² and skips inputs
longer than a kilobyte: every input stays cheap, and the time goes into
variety. Size is a separate test, which folds sets of a quarter of a million
hosts and would take more than a minute if folding were quadratic. A second
target checks that the DHCP parser never panics on any input; run it with
`go test ./internal/dhcp/ -fuzz FuzzParse`.

**A differential corpus** holds node set expressions with the answer
ClusterShell gave for each, in `nodeset/testdata/clustershell.txt`. A test
checks that clusterctl names the same hosts, except on the lines marked as one
of the divergences `doc/nodeset.md` lists, where it checks that the answers
still differ. `clustershell.py` next to it records the answers again.

**A fake BMC** serves the Redfish surface clusterctl uses, over TLS, from
`httptest`. It is how the reset-type check, the once-only action and the boot
override default are covered without hardware.

**A recording transport** stands in for ssh. `transport.Recorder` records what
would have been sent and replies with prepared output, so the subsystems and
the whole command tree can be driven without a cluster. The command tests use
the example configuration that ships with the documentation, which keeps the
example honest.

**Command tests** drive the real command tree end to end and assert on what
would be sent, not on whether the code compiles: that a glob and an apostrophe
survive the trip, that a declined confirmation sends nothing, that a dry run
sends nothing, that a protected host is refused, and that exit codes are what
the contract says.

## What is deliberately not tested

Anything that needs real hardware or a real cluster: IPMI against a service
processor, fabric diagnostics, a Slurm controller, a PXE boot. Those paths are
covered up to the point where a command is handed to the transport — the
command that would be sent is asserted, its effect is not.

That boundary is honest, not convenient. The alternative is mocking a fabric
diagnostic tool's output and testing the mock.

If this is ever taken further, the review that preceded the rewrite names the
backends worth standing up: the DMTF Redfish mockup server, sushy-tools,
OpenIPMI's `ipmi_sim`, a containerised Slurm, and ClusterShell itself as the
reference for node set output.

## Coverage

Coverage is reported per package and is not a target in itself. The packages
that hold the logic sit between 70 and 96 per cent; the command tree is lower
because much of it is the last step before a remote host.

Two rules keep the number meaningful:

- Every package has at least one test file, which is also what keeps
  `go test -cover ./...` working across the module.
- A test asserts on behaviour that would be wrong if it changed, not on the
  shape of an implementation.

## Writing a test here

Table-driven, with the case name saying what property is being checked rather
than which function is being called. A test that needs a reason has a comment
giving it: several tests exist because a specific thing went wrong in the shell
toolkit, and that is worth recording next to the assertion.
