<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# clusterctl design documentation

This directory records how clusterctl is built and why. It is written for
someone who has to change the program, not for someone using it; the user
manual lives in [`../site/`](../site/) and is published at
<https://gsi-hpc.github.io/clusterctl/>.

## What this is

clusterctl replaces a toolkit of 27 Bash scripts that a team used to
administer one or more Slurm clusters. The scripts did six jobs: reach every
infrastructure host, select nodes and fan out commands to them, operate the
service processors, reinstall nodes, administer Slurm, and serve several
clusters from one checkout.

A review of that toolkit found, in order of severity, that commands changed on
the way to the remote host, that destructive actions had no safeguards, that
node sets were passed unquoted, that secrets leaked into remote argument
vectors, that option order changed behaviour, that several features silently
did nothing, that the same flag meant different things in different tools, that
four different SSH paths with different trust settings coexisted, and that
configuration and output were fragile. Every one of those findings has a
counterpart in this design.

## How to read this

| Document | What it covers |
| --- | --- |
| [architecture.md](architecture.md) | The packages, what each owns, and how a command flows through them |
| [configuration.md](configuration.md) | The five document kinds, the merge layers and where a value came from |
| [nodeset.md](nodeset.md) | The node set language and the semantics chosen for it |
| [transport.md](transport.md) | How a command reaches a host and why it is quoted the way it is |
| [safety.md](safety.md) | What a destructive command has to pass before it runs |
| [requirements.md](requirements.md) | What the program has to do, and where each requirement is met |
| [testing.md](testing.md) | What is tested, how, and what cannot be |
| [release.md](release.md) | Versioning, the release workflow and the documentation site |
| [migration.md](migration.md) | Moving a site from the shell toolkit to clusterctl |
| [adr/](adr/) | The decisions, each with its context and consequences |

## Conventions

- Every source file carries `SPDX-License-Identifier: LGPL-3.0-or-later`.
- Packages under `internal/` are implementation; `nodeset/` at the module root
  is the one package offered to other programs.
- A package comment says what the package owns and why it exists, not what its
  functions are called.
- A comment explains a decision or a hazard. Code that needs a comment to say
  what it does is rewritten instead.
