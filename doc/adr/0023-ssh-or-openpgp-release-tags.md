<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0023 — A release tag is signed with an SSH or an OpenPGP key

Status: accepted

It supersedes part of [0007](0007-version-from-signed-tags.md): the cost that
names `RELEASE_ALLOWED_SIGNERS` as the list every release is verified against.

## Context

Since `f66907c` the release workflow verifies every tag with `git verify-tag`
against the SSH keys in the `RELEASE_ALLOWED_SIGNERS` repository variable. It
runs git with an empty GnuPG home, so an OpenPGP signature has no key to verify
against and is refused. That was the safe way to close the holes the check was
written for, not a choice between the two formats.

The maintainer signs with an OpenPGP key elsewhere. v0.1.0 and v0.2.0, cut
before the check, carry OpenPGP signatures, and so does every release of
[sind](https://github.com/GSI-HPC/sind) and
[sind-action](https://github.com/GSI-HPC/sind-action). v0.3.0 had to be signed
with an SSH key instead: a second key, kept for one repository. The modules
[go-nodeset](https://github.com/GSI-HPC/go-nodeset) and
[go-clikit](https://github.com/GSI-HPC/go-clikit), which take their code from
clusterctl, accept both formats with the same check.

## Decision

A release tag is signed either with an SSH key listed in
`RELEASE_ALLOWED_SIGNERS`, or with an OpenPGP key held in
`RELEASE_ALLOWED_PGP_KEYS`, a second repository variable of public keys, ASCII
armored.

- The workflow imports the OpenPGP keys into a GnuPG home that holds nothing
  else, and runs `git verify-tag` with `gpg.minTrustLevel=undefined`: being in
  the variable is the trust, and gpg's web of trust plays no part.
- A signature in a format with no listed key does not verify. Every other check
  stays as it was: the name inside the signed object, the commit it names, and
  the refusal of a lightweight or unsigned tag.
- Either variable may be empty, not both. A variable that holds a private key,
  or no public key at all, stops the release.
- `verify-release-tag_test.sh` makes and checks tags in both formats, and CI
  runs it.

[doc/release.md](../release.md) says how to set up either variable.

## Why

- The maintainer signs clusterctl's releases with the key that signs the other
  GSI-HPC releases, rather than keeping a second one for this repository.
- The check stays where it is and fails closed in the same way: the second
  format adds a list of keys, not an exception.
- The check is the same as go-nodeset's and go-clikit's, so a fix to one
  applies to all three.

## Costs

- gpg becomes part of the release. GitHub's Ubuntu runners carry it.
- The workflow knows an OpenPGP key only as the variable holds it. A key that
  has expired stops verifying on its own, but a revoked one verifies until the
  variable holds its revocation, so revoking a key means exporting it again, or
  removing it.
- Two variables to keep where there was one. A repository variable is no
  secret, since a workflow can print it and GitHub does not mask it in the log,
  so both hold public keys only, and the workflow refuses a private key it
  finds there.
