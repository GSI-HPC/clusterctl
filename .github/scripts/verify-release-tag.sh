#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
# SPDX-License-Identifier: LGPL-3.0-or-later
#
# Refuses a release tag that is not a signed statement, by a listed signer,
# about the commit being released under the name it was pushed as.
#
# The version of a release exists only in its tag, so the tag has to be a
# statement by a person holding a key rather than a label anyone with write
# access can move. Run in a checkout of the pushed tag:
#
#   GITHUB_REF_NAME   the tag that was pushed, v1.4.0
#   GITHUB_SHA        the commit the push event is about
#   ALLOWED_SIGNERS   the SSH keys that may sign a release, in the format of
#                     git's gpg.ssh.allowedSignersFile, from the
#                     RELEASE_ALLOWED_SIGNERS repository variable
#   ALLOWED_PGP_KEYS  the OpenPGP keys that may sign a release, their public
#                     keys ASCII armored, from the RELEASE_ALLOWED_PGP_KEYS
#                     repository variable
#   GITHUB_OUTPUT     optional; receives object= and commit=, the tag object
#                     and the commit that were verified.
#
# The keys come from repository variables and never from the checkout, which
# the tagged commit controls. Either list may be empty, not both.
#
# Every check fails closed: no listed key, a signature git cannot verify, and
# a signature by anyone else all stop the release.

set -euo pipefail

tag="${GITHUB_REF_NAME:?the tag to verify}"
sha="${GITHUB_SHA:?the commit of the push event}"
signers="${ALLOWED_SIGNERS:-}"
pgp_keys="${ALLOWED_PGP_KEYS:-}"

fail() {
  echo "::error::$*"
  exit 1
}

if [ -z "${signers//[[:space:]]/}" ] && [ -z "${pgp_keys//[[:space:]]/}" ]; then
  fail "the RELEASE_ALLOWED_SIGNERS and RELEASE_ALLOWED_PGP_KEYS repository variables are empty or not set; releases are verified against them, see doc/release.md"
fi

object="$(git rev-parse --verify --quiet "refs/tags/$tag")" ||
  fail "$tag is not a tag in this checkout"

if [ "$(git cat-file -t "$object")" != "tag" ]; then
  echo "::error::$tag is a lightweight tag; releases are cut from signed annotated tags"
  echo "  git tag -s $tag -m 'clusterctl $tag'"
  exit 1
fi

# The signature, checked by git against the listed keys alone: the SSH keys
# in an allowed signers file, and the OpenPGP keys in a GnuPG home that holds
# nothing else, so that a good signature is one of theirs whatever trust gpg
# gives it. A signature in a format with no listed key does not verify.
work="$(mktemp -d)"
cleanup() {
  # gpg starts an agent for the home it is given; it goes with the home.
  gpgconf --homedir "$work/gnupg" --kill all > /dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
printf '%s\n' "$signers" > "$work/allowed_signers"
mkdir -m 0700 "$work/gnupg"

if [ -n "${pgp_keys//[[:space:]]/}" ]; then
  # A repository variable is no secret: anyone who can run a workflow can
  # print it.
  if grep -qF -- '-----BEGIN PGP PRIVATE KEY BLOCK-----' <<< "$pgp_keys"; then
    fail "RELEASE_ALLOWED_PGP_KEYS holds a private key; remove it, and revoke the key"
  fi
  GNUPGHOME="$work/gnupg" gpg --batch --quiet --import <<< "$pgp_keys" ||
    fail "RELEASE_ALLOWED_PGP_KEYS holds something gpg cannot import; it takes public keys, ASCII armored"
  keys="$(GNUPGHOME="$work/gnupg" gpg --batch --with-colons --list-keys)"
  grep -q '^pub:' <<< "$keys" ||
    fail "RELEASE_ALLOWED_PGP_KEYS holds no OpenPGP public key"
fi

if ! GNUPGHOME="$work/gnupg" git -c gpg.ssh.allowedSignersFile="$work/allowed_signers" \
  -c gpg.minTrustLevel=undefined verify-tag "$object"; then
  fail "$tag is not signed by a key in RELEASE_ALLOWED_SIGNERS or RELEASE_ALLOWED_PGP_KEYS"
fi

# A signature covers the name inside the tag object, not the ref it was pushed
# under: a signed v1.0.0 pushed again as v9.9.9 verifies. The name is the
# "tag" header, which ends at the first blank line; the message after it is
# not looked at.
name="$(git cat-file tag "$object" | sed -n '/^$/q; s/^tag //p')"
if [ "$name" != "$tag" ]; then
  fail "the signed tag is named '$name' but was pushed as $tag"
fi

# The commit the event names is the one that was signed. GitHub reports the
# commit an annotated tag points at; the tag object itself names it as well.
commit="$(git rev-parse --verify "$object^{commit}")"
if [ "$sha" != "$commit" ] && [ "$sha" != "$object" ]; then
  fail "$tag points at $commit, but the push was of $sha"
fi

echo "::notice::$tag ($object) is signed by a listed signer and names $commit"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "object=$object"
    echo "commit=$commit"
  } >> "$GITHUB_OUTPUT"
fi
