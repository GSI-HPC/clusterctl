<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0011 — Release assets are consumable by a version manager

Status: accepted

## Context

An administrator's workstation carries a set of pinned tools, and an HPC team
that already runs [mise](https://mise.jdx.dev) or a similar manager wants
clusterctl in it rather than a binary copied into `~/.local/bin` by hand and
never updated.

mise installs from GitHub release assets through its `github:` backend, which
picks the asset for the host by scoring the name for operating system,
architecture, libc and archive format, then finds the executable in what it
extracted. Nothing in a project's own repository configures this: the release
assets are the interface.

## Decision

Keep the release assets shaped so that the `github:` backend resolves them
with no options, and publish the evidence it can verify. Do not pursue a
registry shorthand.

```console
$ mise use -g github:GSI-HPC/clusterctl
```

## What that requires of a release

- **One archive per platform, with the platform in the name.**
  `clusterctl_linux_amd64.tar.gz` and its three siblings. `amd64`, `arm64`,
  `linux` and `darwin` are all names the matcher recognises.
- **The executable at the root of the archive**, named as the tool is invoked.
  mise uses the root of the extracted archive when an executable is there,
  which it is; the manual pages and the example configuration alongside it are
  ignored.
- **`checksums.txt`**, which the matcher finds by name and uses to check the
  download and to fill a consumer's `mise.lock`.
- **A build provenance attestation**, which mise verifies by default when one
  exists. `actions/attest` is given `checksums.txt`, so one call covers every
  artifact of the release.

None of this is specific to mise. The same shape is what `gh release download`,
an ansible task and a Homebrew formula all want, and the attestation is
verifiable with `gh attestation verify` by anyone.

## Why not a registry shorthand

`mise use clusterctl`, without the backend prefix, needs an entry in mise's own
registry. That registry takes tools that are "already widely used, normally
with thousands of GitHub stars". A new tool for one site's administrators is
not that, and submitting it would waste a maintainer's time. The backend prefix
costs one word.

## Why not packslip

[packslip](https://packslip.dev) is mise's preferred tier-1 backend: the
publisher signs a release manifest, and mise verifies the signer, the artifact
digests, and installs version-matched shell completions along the way. That
last part is genuinely attractive, because completion here is generated from
the command tree and should follow the binary.

It is not done yet because it adds a publishing tool and a signing identity to
a release process that currently has one signed tag and one workflow, and
because the `github:` backend already covers installing and verifying. It is
the obvious next step if the completions become awkward to distribute, and this
record should be superseded rather than edited when that happens.

## Costs

- The archive name carries no version, so that
  `releases/latest/download/clusterctl_linux_amd64.tar.gz` is a stable URL.
  A downloaded file therefore does not say which version it is until it is run.
  The version is in the tag, in the release and in `clusterctl version`.
- Attestation needs `id-token: write` and `attestations: write` on the release
  job, and works in public repositories on any current GitHub plan. In a
  private repository it needs GitHub Enterprise Cloud; without it the release
  still publishes and `checksums.txt` still verifies, and consumers can set
  `github_attestations = false` for the tool.
