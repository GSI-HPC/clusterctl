<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0015 — Release notes are written in the signed tag

Status: accepted

## Context

The release notes were the commits since the previous tag, grouped by type.
That list says what changed, but not what a release means to someone deciding
whether to take it, and the first release shows its limits outright: there is
no previous tag, so the list would be every commit in the repository,
beginning with the shell toolkit clusterctl replaced.

The words have to live somewhere. [ADR 0007](0007-version-from-signed-tags.md)
rules out a file in the tree that names a version, so a `CHANGELOG.md` is out.
Editing the release on GitHub after the workflow publishes it is possible, but
it is a step outside the release process and anyone with write access can make
it.

## Decision

The body of the tag message opens the release notes: everything after its
first line, placed under the release heading by `.goreleaser.yaml`. The
generated commit list follows it, except on the first release, where there is
no previous tag and the tag message stands alone.

A tag made with `-m` has no body, and the notes are the commit list as before.

## Why

- The notes are signed by the same key and in the same act as the version, so
  the announcement is as much a statement by the person releasing as the tag.
- Nothing is added to the release process: no file, no second step, no edit
  after publishing. A release with nothing to say costs nothing.
- GoReleaser reads the body with `%(contents:body)`, which stops before the
  signature, so the signature never ends up in the notes.

## Costs

- git's default message cleanup deletes every line that starts with `#`, and
  Markdown headings are such lines. The message has to be written to a file and
  tagged with `--cleanup=whitespace`; `doc/release.md` says so.
- The notes cannot be corrected without moving the tag, which the signature
  rules out. A mistake is fixed by editing the published release, and that
  edit is no longer signed.
