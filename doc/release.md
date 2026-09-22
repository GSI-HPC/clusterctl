<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Versioning, releases and the documentation site

## No version in the source tree

Nothing in this repository says what version it is. There is no `VERSION` file,
no constant and no commit that bumps a number.

A version exists only in two places: a **signed git tag**, and the GitHub
release object built from it. That is deliberate — a version in the tree is a
claim anyone can edit and that is wrong on every commit between releases,
whereas a signed tag is a statement by a person with a key.

`internal/version` therefore reports:

- the version injected by the release build through `-ldflags`, when there was
  one;
- otherwise whatever the Go toolchain stamped into the binary from the VCS: the
  revision, the commit time, and whether the tree was dirty;
- otherwise `devel`.

```
$ clusterctl version
v1.4.0 (a1b2c3d4e5f6) built 2026-09-22T14:42:30Z go1.26.0 linux/amd64
```

## Cutting a release

```
git tag -s v1.4.0 -m 'clusterctl v1.4.0'
git push origin v1.4.0
```

The tag must be **signed**: the release workflow refuses an unsigned or
lightweight tag before it builds anything. With `.github/allowed_signers`
present, the signature is verified against it; without it, the workflow checks
that a signature is there at all and says so in the job log.

The workflow then builds, tests, and publishes:

- `clusterctl_<version>_linux_amd64.tar.gz` — the binary asked for by the
  review, statically linked with `CGO_ENABLED=0`
- the same for `linux/arm64`, `darwin/amd64` and `darwin/arm64`
- `checksums.txt` and its signature
- release notes generated from the commits since the previous tag

The binary is built with `-trimpath` and with the version, commit and build
date injected, so two builds of the same tag produce the same binary.

## Version numbers

Semantic versioning, with the command line as the public interface:

- **major** — a command, a flag or an exit code changes meaning or goes away
- **minor** — a command or a flag is added, or a configuration field is added
- **patch** — a fix that changes no interface

The configuration schema carries its own version, `clusterctl/v1alpha1`. As an
alpha version it promises nothing; a document written for a later version is
reported on load rather than misread.

## The documentation site

The manual is a [Hugo](https://gohugo.io) site under `site/`, using the
[Hextra](https://imfing.github.io/hextra/) theme. Hugo is a Go program
installed as a single binary, Hextra is a Hugo module, and neither needs Node
or a package manager — which is why this rather than the alternatives.

Two kinds of page live there:

- **Written by hand**: everything an administrator reads. The manual is not
  generated from help text, because help text and a manual answer different
  questions.
- **Generated**: the command reference, produced from the cobra tree by
  `internal/tools/gendocs`, and the JSON Schemas. Both are built in CI and are
  not committed, so they cannot drift from the binary.

`make docs` regenerates them locally. `hugo server` in `site/` then serves the
manual at <http://localhost:1313>.

The site is published to GitHub Pages on every push to the default branch and
on every release, by `.github/workflows/pages.yml`.
