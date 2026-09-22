<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0009 — Hugo and Hextra for the manual

Status: accepted

## Context

The manual is published on GitHub Pages alongside the repository. The
requirement was a site generator that is idiomatic for a Go command line tool.

Candidates: Hugo, MkDocs with Material, Docusaurus, and mdBook.

## Decision

Hugo with the [Hextra](https://imfing.github.io/hextra/) theme, in `site/`.

## Why

- **Hugo is a single Go binary.** No Python environment, no Node, no package
  manager. The CI job installs one file and runs it, and a contributor can
  build the manual on any machine that can build clusterctl.
- **Hextra is a Hugo module.** `hugo mod get` fetches it through the same
  module proxy as the Go dependencies. Docsy, the other obvious choice, needs
  npm and PostCSS, which puts a JavaScript toolchain in the release path of a
  Go program for no gain.
- **It is what Go projects use.** Hugo is itself written in Go and is the
  common choice in this ecosystem, so the tooling is familiar to whoever
  maintains this next.
- **The command reference can be generated into it.** `internal/tools/gendocs`
  walks the cobra tree and writes Markdown that Hugo renders with the rest.

## What is written and what is generated

The manual is **written by hand**. Help text answers "what are the flags"; a
manual answers "how do I reinstall a rack", and generating one from the other
produces neither.

The command reference and the JSON Schemas are **generated in CI** and are not
committed, so they cannot drift from the binary they describe.

## Costs

- Hextra pins a Hugo version range; a Hugo upgrade can need a theme upgrade.
  Both are pinned in the workflow.
- Hugo's module system needs network access at build time, and Hextra's search
  index fetches FlexSearch from a CDN while the site is built. The pages
  workflow has both; an air-gapped build would need the theme vendored and
  `params.search.enable` set to false.
