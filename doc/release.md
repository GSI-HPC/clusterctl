<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Versioning, releases, the documentation site and dependencies

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
v1.4.0 (a1b2c3d4e5f6) built 2026-09-22T14:42:30Z go1.26.8 linux/amd64
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

- `clusterctl_linux_amd64.tar.gz` — the binary asked for by the review,
  statically linked with `CGO_ENABLED=0`
- the same for `linux/arm64`, `darwin/amd64` and `darwin/arm64`
- `checksums.txt`, covering all four
- a build provenance attestation for every artifact
- release notes generated from the commits since the previous tag

The archive name carries no version, so
`releases/latest/download/clusterctl_linux_amd64.tar.gz` is a stable URL. The
version is in the tag, the release and the binary itself.

The binary is built with `-trimpath` and with the version, commit and build
date injected, so two builds of the same tag with the same toolchain produce
the same binary.

## The Go toolchain

`go.mod` says `go 1.26.0`: the oldest Go that compiles the module, which is
what someone running `go install` needs to know. The workflows build with the
newest patch of the 1.26 line instead — `GO_VERSION` in each workflow, resolved
with `check-latest` — because Go ships security fixes to the standard library
as patch releases, and a binary is only as patched as the toolchain that linked
it. `govulncheck` runs in CI against that same toolchain, so a vulnerability
reachable from clusterctl fails the build until a patch release fixes it.

`clusterctl version` reports the toolchain a binary was built with. Moving to
the next Go release line means changing `GO_VERSION` in the three workflows and
`go` in `mise.toml`; raising the minimum means changing `go.mod`.

## Keeping dependencies current

Dependabot opens the pull requests, weekly, as `.github/dependabot.yml`
configures and [ADR 0012](adr/0012-dependabot.md) explains:

- `build(deps)` for the requirements in `go.mod`, with the minor and patch
  releases grouped into one pull request;
- `ci(deps)` for each action the workflows use.

It proposes a release once it has been out for a week, and a security update at
once. The pull requests run CI like any other. An action used only by the
release workflow is not exercised until the next release, so read its release
notes before merging.

The rest is updated by hand:

| What | Where | How |
| --- | --- | --- |
| The Hextra theme | `site/go.mod` | `hugo mod get -u github.com/imfing/hextra`, then `hugo mod tidy`, in `site/`. Never `go mod tidy`: it removes the requirement. |
| Hugo | `hugo-version` in `ci.yml` and `pages.yml` | Change both, and the minimum in `site/README.md`. |
| The Go release line | `GO_VERSION` in the workflows, `go` in `mise.toml` | As described under [the Go toolchain](#the-go-toolchain). |

golangci-lint and govulncheck need nothing, and neither does GoReleaser within
its major version: the workflows take their latest release every time.

## Provenance

Every artifact is attested with `actions/attest`, which binds its digest to the
workflow, the commit and the tag that produced it, and signs that statement with
a short-lived Sigstore certificate. Nothing has to be published for a consumer
to check it:

```console
$ gh attestation verify clusterctl_linux_amd64.tar.gz --repo GSI-HPC/clusterctl
```

`checksums.txt` answers "are these the bytes that were published"; the
attestation answers "who published them, from which source". A download is
worth both.

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

Publishing needs Pages switched on once per repository, with GitHub Actions as
its source: *Settings → Pages → Build and deployment → Source*. The workflow
does not switch it on itself, because that needs a token other than the
`GITHUB_TOKEN` it runs with. Until it is on, the build job stops at
`actions/configure-pages` with "Get Pages site failed".
