<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0006 — No compatibility layer for the shell toolkit

Status: accepted

## Context

The review suggests keeping the old `cluster-*` names as compatibility entry
points, a `shell-init` command to recreate the `cl`, `cli`, `rush` and `crush`
aliases, and a `config migrate` importer for the old `.conf`, CSV, genders and
sshuttle files.

## Decision

None of it. clusterctl is the only interface, and a site moves by writing a new
configuration.

## Why

The old flags **are** the problem the rewrite exists to solve. The review's own
findings: `-d` means debug in most tools and "log in to the database host as
root" in `cluster-login`; `-D` means debug, date or database depending on the
tool; `-r` before `-i` silently resets the user so `--root --mgmt` does not run
as root; `-L` is accepted and ignored, so it ends up in the remote command.

A shim that accepts those flags has to reproduce that behaviour, including the
parts that are bugs, and then every user of the shim keeps learning the old
semantics. The inconsistency would outlive the toolkit that caused it.

The importer is a smaller case of the same thing. The old formats encode
assumptions — one domain per prefix, one node set per line, a boot path table
keyed by nodeset — that the new configuration deliberately does not share. An
importer would produce a configuration shaped like the old one, and a site
would keep it.

## Costs

- A site does a conversion by hand. `examples/site/` is a complete
  configuration built from exactly these files, and
  [../migration.md](../migration.md) maps every command, flag and variable, so
  it is transcription rather than design.
- Muscle memory goes. `cl` and `rush` no longer exist. A site that wants them
  can define shell aliases; clusterctl will not install them, because a tool
  that modifies a shell is a tool that has to be debugged inside one.
- A script calling the old tools breaks rather than changing meaning. That is
  the intended failure: a loud break beats a quiet difference.
