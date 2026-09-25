<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0020 — One exit code rule for a command on many hosts

Status: accepted

## Context

The exit codes are part of the command line contract, and the manual says
which one a command on many hosts exits with when its hosts failed in
different ways: the first that applies of `130`, `3` and `1`. The review of
issue #90 found that rule implemented five times, and not the same way:

| One run in which… | exec, copy | cinc | secrets push | provision | bmc |
| --- | --- | --- | --- | --- | --- |
| one refused, one unreachable | 3 | 3 | 1 | 3 | 3 |
| one interrupted, one unreachable | 130 | 3 | 3 | 130 | 130 |
| one interrupted, one refused | 130 | 130 | 1 | 130 | 130 |
| one without configuration, one refused | 1 | 1 | 1 | 2 | 2 |
| one without configuration, one unreachable | 3 | 3 | 1 | 3 | 3 |

`secrets push` exited `3` only when no node had answered. `cinc` let an
unreachable node win over an interrupt. `exec`, `copy` and `cinc` counted a
node the configuration could not serve, such as one whose host name is not a
host name, as a node that refused. The contract did not say where `2` goes.

## Decision

One function, `exitcode.Worst`, gives the code of every command that acts on
several hosts, and the manual states it: the first that applies of `130` (a
cancellation counts as one however it was wrapped), `3`, `2` and `1`. A
failure that carries no error, such as a command that exited non-zero, is a
`1`.

The commands whose result is what they find keep their own rule, and the
manual lists them: `hostkey verify` and `hostkey refresh` exit `1` for a
changed or revoked key before `3` for a host that did not answer, and
`bmc ping` and `dns` exit `1` for a processor that does not answer and a
name that does not resolve.

## Why

- A script reads one code per run. It can only branch on it if every
  command means the same thing by it.
- `3` before `1` is what the contract already promised, and what four of the
  five did. A node that was not there may have refused too; "check the
  network first" is the step that comes first.
- `2` before `1`: a node the configuration cannot serve is a mistake in the
  input, which no retry fixes, and says more than a node that said no. It
  comes after `3` because the network is checked first, as above.
- For a command that looks for something, what it found is the answer to
  the question asked. A changed host key must not hide behind a host that is
  down, where a script reading `3` would retry later and miss it.

## Costs

- `secrets push` exits `3` where it exited `1` when one node refused and
  another was down, and `exec`, `copy`, `cinc`, `fabric hca` and
  `node hardware` exit `2` where they exited `1` when a node's configuration
  was at fault. Both are changes in meaning of an existing code for those
  runs.
- The exceptions are a second rule to know. They are few, and each is a
  command whose purpose is the finding.
