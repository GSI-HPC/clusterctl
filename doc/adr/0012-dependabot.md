<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0012 — Dependabot proposes the dependency updates

Status: accepted

## Context

clusterctl depends on things of five kinds, each pinned in its own place:

- **Go modules**, in `go.mod`, among them `golang.org/x/crypto` and
  `filippo.io/age`, which scan host keys and decrypt secrets.
- **GitHub Actions**, in the workflows. Their major versions carry the Node
  runtime they run on, and GitHub retires old runtimes on its own schedule.
- **The Hextra theme**, a Hugo module in `site/go.mod`.
- **Tools the workflows install**: Hugo at a fixed version; golangci-lint,
  govulncheck and GoReleaser at their latest release, GoReleaser within its
  major version.
- **The Go release line**, in `GO_VERSION` and `mise.toml`.

Left alone, all of these age, and the first sign is a vulnerability report or
a runner that stops supporting an action.

## Decision

Dependabot proposes version updates every week, configured in
`.github/dependabot.yml`, for:

- **the requirements in `go.mod`** — minor and patch releases grouped into one
  pull request, because CI runs the whole suite on it;
- **the actions the workflows use** — one pull request per action, because the
  release and Pages workflows do not run on pull requests, so a new major
  version of one of their actions has to be reviewed on its release notes.

A release is proposed after it has been out for seven days, so that a
compromised or broken release has time to be found out first. Security updates
do not wait. Commits follow Conventional Commits, as `build(deps)` and
`ci(deps)`.

## What it does not cover

- **The Hugo module.** Dependabot finishes a Go module update with
  `go mod tidy`. A Hugo module has no Go packages, so tidying it removes every
  requirement, and the pull request would unpin the theme instead of updating
  it. `hugo mod get -u` does the update, and `hugo mod tidy` cleans up after it
  without that problem.
- **Hugo.** Its version is an action input, `hugo-version`, which Dependabot
  does not read.
- **The Go release line.** Moving to the next one is a decision about
  supported toolchains, not an update; `doc/release.md` describes it.
- **`mise.toml`.** Dependabot has no ecosystem for it, and its entries name
  release lines that mise resolves to their newest release anyway.

The tools that track their latest release need nothing.

## Why not Renovate

Renovate would cover more of this: it can update a Go module without tidying
it, it reads `mise.toml`, and a custom manager can be taught version strings
such as `hugo-version`. It is also an application to install, or a job to run,
with a configuration language of its own. Dependabot is part of GitHub and
needs one file, and what it leaves out is short and written down.

## Costs

- Pull requests someone has to merge: one a week for the minor and patch
  releases of the Go modules, one for each major release, and one for each
  action.
- An update to an action only the release workflow uses is exercised by the
  next release, not by its pull request.
- The Hextra theme, Hugo and the Go release line are updated by hand, as
  `doc/release.md` describes.
- Dependabot alerts and security updates are switched on in the repository's
  security settings, not by this file.
