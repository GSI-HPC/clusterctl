<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0007 — The version lives only in a signed tag

Status: accepted

## Context

A release needs a version number. The usual places to keep one are a file in
the tree, a constant in the source, or a tag.

## Decision

No version anywhere in the repository. The version exists in a signed git tag
and in the GitHub release built from it, and nowhere else.

## Why

- A number in the tree is wrong on every commit between releases, and it is a
  claim anyone with write access can make. A signed tag is a statement by a
  person holding a key, at a point in history, that cannot be moved without
  being noticed.
- There is no release commit to forget, no merge conflict on a version line,
  and no way for the tree to disagree with the tag.
- The release workflow refuses an unsigned or lightweight tag before it builds
  anything, so an unsigned release is not possible by accident.

## How a build knows its version

`internal/version` takes, in order:

1. the value injected through `-ldflags` by the release build;
2. otherwise the VCS stamps the Go toolchain embeds — revision, commit time,
   and whether the tree was dirty;
3. otherwise `devel`.

A build from a checkout therefore reports its revision honestly rather than
claiming a version it is not:

```
devel (a1b2c3d4e5f6-dirty) built 2026-09-22T14:42:30Z go1.26.0 linux/amd64
```

## Costs

- `go install` of an untagged commit produces a pseudo-version rather than a
  release number. That is accurate, and it is what the release asset is for.
- The signing key becomes part of the release process. Without
  `.github/allowed_signers` the workflow can only check that a signature is
  present, not whose it is; with it, the signer is verified.
