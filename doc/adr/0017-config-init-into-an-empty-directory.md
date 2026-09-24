<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0017 — `config init` writes only into an empty directory, where it is read

Status: accepted

## Context

`clusterctl config init [DIR]` writes the least configuration that resolves: a
`Config`, a `Site`, a `Cluster` and a `NodeInventory`, one document to a file.
Two things have to be settled: where the files go when no `DIR` is given, and
which directories the command may write into at all.

Configuration is read from what `--config` or `CLUSTERCTL_CONFIG` names, and
only when neither is set from `/etc/clusterctl` and the user's configuration
directory. Files written anywhere else are not found by the next command
until a variable points at them, and setting `CLUSTERCTL_CONFIG` for that
replaces whatever it said before.

A directory may already hold things: a configuration, a README, a host key
file, `.sops.yaml` rules, a `.git` directory. Telling which of them matter
would make the command judge files it knows nothing about, and tie that
judgement to the loader's rule for what counts as a configuration file, so
that the two would have to change together.

## Decision

**Where.** The directory given as `DIR`. Without one, the place configuration
is read from:

1. the directory `--config` names,
2. else the directory `CLUSTERCTL_CONFIG` names,
3. else the user's configuration directory, the one the search path reads
   after `/etc/clusterctl`: on Linux `$XDG_CONFIG_HOME/clusterctl`, usually
   `~/.config/clusterctl`.

When `--config` or `CLUSTERCTL_CONFIG` names several places, the command
refuses and asks for `DIR`: the places are read together, most general first,
and a complete configuration written into one of them changes what the others
resolve to. The default search path is several places too. The command
refuses in the same way when it would write into the user's configuration
directory and another directory of the search path, `/etc/clusterctl`, holds
configuration: the scaffold would replace a team's documents of the same name
or move the current context to its own site, and with either the team's
protected hosts would stop applying. Directories that are missing, parents included, are created.

**Into what.** A directory that does not exist yet or is empty. Any entry, a
hidden file or a subdirectory included, makes the command refuse before it
creates or writes anything. `--dry-run` refuses the same way, so it predicts
the real run.

## Why

- "Empty" is a rule a person can check with `ls -A`, and it does not change
  when the loader learns to read another kind of file.
- The command never decides whether something it did not write matters. What
  is there was put there by someone, for a reason the command cannot see.
- The files land where the next command reads them. The next step is
  `config validate`, not an environment variable to set.
- Together with files created exclusively and removed again when a later one
  fails, nothing a person wrote is ever changed, overwritten or mixed with the
  scaffold.

## Costs

- A site repository that exists before its configuration is refused: a clone
  holds `.git`, and often a README. Write the scaffold into an empty
  subdirectory of it, or run `config init` first and `git init` afterwards.
- Someone whose `CLUSTERCTL_CONFIG` layers several directories names the one
  to write to on every `config init`, and so does someone on a host whose
  `/etc/clusterctl` holds a configuration.
- On macOS the user's configuration directory is
  `~/Library/Application Support/clusterctl`, because that is what Go's
  `os.UserConfigDir` returns and what the search path already reads.
  `~/.config/clusterctl` is used there only when `DIR`, `--config` or
  `CLUSTERCTL_CONFIG` names it.
