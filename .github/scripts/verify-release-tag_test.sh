#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
# SPDX-License-Identifier: LGPL-3.0-or-later
#
# Tests verify-release-tag.sh against tags made in a scratch repository, one
# case per way a tag has been shown to pass that should not. The release
# workflow runs only on a tag push, so this is where its first step is tested.
#
# Needs git and ssh-keygen. Run from anywhere:
#
#   .github/scripts/verify-release-tag_test.sh

set -euo pipefail

check="$(cd "$(dirname "$0")" && pwd)/verify-release-tag.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=Maintainer GIT_AUTHOR_EMAIL=maintainer@example.org
export GIT_COMMITTER_NAME=Maintainer GIT_COMMITTER_EMAIL=maintainer@example.org

ssh-keygen -q -t ed25519 -N '' -C listed -f "$scratch/listed"
ssh-keygen -q -t ed25519 -N '' -C unlisted -f "$scratch/unlisted"
listed="maintainer@example.org namespaces=\"git\" $(cat "$scratch/listed.pub")"
unlisted="intruder@example.org namespaces=\"git\" $(cat "$scratch/unlisted.pub")"

repo="$scratch/repo"
git init -q "$repo"
cd "$repo"
git commit -q --allow-empty -m 'first'
first="$(git rev-parse HEAD)"

# Signs a tag with the given key: sign <key> <git tag arguments...>
sign() {
  local key="$1"
  shift
  git -c gpg.format=ssh -c user.signingKey="$scratch/$key" tag -s "$@"
}

failures=0

# run <name> <want: pass|fail> <tag> <sha> <signers> [<pattern in output>]
run() {
  local name="$1" want="$2" tag="$3" sha="$4" signers="$5" pattern="${6:-}"
  local out got=pass
  out="$(GITHUB_REF_NAME="$tag" GITHUB_SHA="$sha" ALLOWED_SIGNERS="$signers" \
    GITHUB_OUTPUT="$scratch/output" "$check" 2>&1)" || got=fail
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: want $want, got $got"
    printf '%s\n' "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  elif [ -n "$pattern" ] && ! grep -qF -- "$pattern" <<< "$out"; then
    echo "FAIL $name: output does not say '$pattern'"
    printf '%s\n' "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

# The release as it is meant to be cut.
sign listed v1.0.0 -m 'clusterctl v1.0.0'
: > "$scratch/output"
run 'a tag signed by a listed signer' pass v1.0.0 "$first" "$listed" 'is signed by a listed signer'
if ! grep -qx "object=$(git rev-parse refs/tags/v1.0.0)" "$scratch/output" ||
  ! grep -qx "commit=$first" "$scratch/output"; then
  echo "FAIL the verified tag object and commit are not in GITHUB_OUTPUT"
  failures=$((failures + 1))
fi
run 'the push event naming the tag object' pass v1.0.0 "$(git rev-parse refs/tags/v1.0.0)" "$listed"
run 'a signer list with a comment and blank lines' pass v1.0.0 "$first" \
  "$(printf '# release signers\n\n%s\n' "$listed")"

# 13.1: the signer list comes from outside the checkout, and none is a failure.
run 'no signer list' fail v1.0.0 "$first" '' 'RELEASE_ALLOWED_SIGNERS'
run 'a blank signer list' fail v1.0.0 "$first" $'  \n' 'RELEASE_ALLOWED_SIGNERS'

mkdir .github
printf '%s\n' "$unlisted" > .github/allowed_signers
git add .github/allowed_signers
git commit -q -m 'add my key'
added="$(git rev-parse HEAD)"
sign unlisted v1.0.1 -m 'clusterctl v1.0.1'
run 'a tagged commit listing its own signer' fail v1.0.1 "$added" "$listed" 'not signed by anyone'

git rm -q .github/allowed_signers
git commit -q -m 'drop the signer list'
dropped="$(git rev-parse HEAD)"
sign unlisted v1.0.5 -m 'clusterctl v1.0.5'
run 'a tagged commit deleting the signer list' fail v1.0.5 "$dropped" "$listed" 'not signed by anyone'

# 13.2: the name inside the signed object is the name pushed.
git update-ref refs/tags/v9.9.9 "$(git rev-parse refs/tags/v1.0.0)"
run 'a signed tag pushed under another name' fail v9.9.9 "$first" "$listed" "named 'v1.0.0' but was pushed as v9.9.9"

sign listed v1.1.0 -m 'clusterctl v1.1.0' -m 'tag v9.9.8' "$first"
git update-ref refs/tags/v9.9.8 "$(git rev-parse refs/tags/v1.1.0)"
run 'a tag message naming the pushed ref' fail v9.9.8 "$first" "$listed" "named 'v1.1.0'"

sign listed v1.2.0 -m 'clusterctl v1.2.0' "$first"
run 'a tag of another commit than the pushed one' fail v1.2.0 "$dropped" "$listed" "points at $first"

# 13.4: a signature is what git verifies, not text that looks like one.
git tag -a v1.3.0 "$first" -F - << 'MSG'
clusterctl v1.3.0

-----BEGIN SSH SIGNATURE-----
not a signature
-----END SSH SIGNATURE-----
MSG
run 'an unsigned tag quoting a signature' fail v1.3.0 "$first" "$listed" 'not signed by anyone'

git tag -a v1.4.0 -m 'clusterctl v1.4.0' "$first"
run 'an unsigned annotated tag' fail v1.4.0 "$first" "$listed" 'not signed by anyone'

git tag v1.5.0 "$first"
run 'a lightweight tag' fail v1.5.0 "$first" "$listed" 'lightweight'

run 'a tag that does not exist' fail v1.6.0 "$first" "$listed" 'not a tag'

if [ "$failures" -ne 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
