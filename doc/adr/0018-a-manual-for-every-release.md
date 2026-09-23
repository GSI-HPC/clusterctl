<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0018 — A manual for every release, the latest at the root

Status: accepted

## Context

The Pages workflow published the manual of main. main runs ahead of the
release people install, so the manual described what they did not have yet:
its getting-started section started from `clusterctl config init`, which
v0.1.0, the only release, does not have. The JSON Schemas at the root, which
editors fetch for the `$schema` line of every configuration file, described
main as well.

The workflow was also meant to republish on every release, and never did.
GoReleaser publishes the release with the `GITHUB_TOKEN`, and GitHub starts no
workflow for an event that token causes.

## Decision

The site holds a manual per version:

| Path | Manual | Search engines |
| --- | --- | --- |
| `/` | the latest release | indexed |
| `/vX.Y/` | the newest patch release of each minor line, the latest's included | not indexed |
| `/dev/` | main | not indexed |

A switcher beside the title lists them, each link leading to the same page in
that version. Every manual but the latest release's carries a banner saying
which one it is, linking to the same page in the latest.

`site/build.sh` builds all of them, every time, and `actions/deploy-pages`
publishes the result as before. The Pages workflow runs on a push to main and
when the release workflow succeeds. That run is on the default branch, which
the `github-pages` environment lets deploy as GitHub sets it up; a run on the
tag would be refused.

## How a version is built

- **The versions are the published releases**, drafts and pre-releases left
  out, not the tags. A tag the release workflow refused never becomes a
  manual, and the latest is what `releases/latest` and mise install.
- **One manual per minor line.** A patch release changes no interface, so
  each minor line has one manual: its newest release's.
- **A release's manual is built from its tag**: its pages, its command
  reference and schemas from its own `gendocs`, the Hextra its `site/go.mod`
  requires, and the Hugo its `site/hugo-version` names. Moving Hugo or the
  theme forward on main cannot break the manual of an earlier release. v0.1.0
  predates `site/hugo-version` and is built with 0.152.0, which its workflow
  named.
- **The switcher and the banner are templates in `site/layouts/`**, so each
  tag carries the ones it was released with; a tag from before them, v0.1.0,
  is given main's. What the build hands them is data, in a configuration file
  of its own: the versions, the latest release, and the version being built.
  The shape of that data is the interface between `site/build.sh` and every
  tag's templates, and changes only compatibly.
- **The latest release is built twice**, at `/` and at `/vX.Y/`, so that a
  link to its minor line keeps working once the next release takes the root.
  The release notes link there.

## Why

- **Nothing generated is committed.** The common alternative, mike and a
  `gh-pages` branch that accumulates builds, keeps generated HTML in the
  repository, needs a token that can push, and freezes every version as it was
  built, so a fix to the switcher never reaches the old ones. Building from the
  tags derives every page from signed sources, as the release does.
- **No state between deployments.** Each run builds the whole site from the
  list of releases and main, so whichever run finishes last publishes the
  right site, and a run that fails leaves the previous one live.
- **No script of our own.** Every build knows the whole list, so the switcher
  is links written at build time rather than a `versions.json` fetched by the
  browser. It uses the markup and the script of Hextra's own dropdown menus,
  but unlike those it is not hidden on a phone.
- **The root is what people install.** The README, the search engines and the
  `$schema` line of every configuration file point at the root, and now find
  the latest release there.
- **Hextra's own documentation is built the same way.**

## Costs

- The build grows with every minor line by a `gendocs` run, which compiles
  clusterctl, and a Hugo run. CI builds the same site on every pull request
  and pays it too. A tag's generated pages never change, so they can be cached
  when that matters.
- A manual is about 29 MB, 24 MB of it the command reference, and GitHub Pages
  publishes at most 1 GB: some thirty manuals. When that comes near, the
  oldest minor lines go.
- Hextra has no hook in its navbar, so `site/layouts/_partials/navbar-title.html`
  replaces the theme's. A theme upgrade has to be checked against it.
- A page the other version does not have leads to the 404 page.
- A manual is only as buildable as its tag: the Hugo release, the Hextra
  module and the tag itself have to stay fetchable. They live on GitHub, the
  Go module proxy and in this repository.
- Hugo merges an extra configuration file into the site's, but replaces lists
  rather than merging them. The versions therefore cannot be menu entries,
  which would replace the site's menu, and a site-wide `cascade` in
  `hugo.yaml` would be replaced by the one that keeps search engines out of
  the older manuals.
