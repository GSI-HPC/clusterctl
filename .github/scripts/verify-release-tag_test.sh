#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
# SPDX-License-Identifier: LGPL-3.0-or-later
#
# Tests verify-release-tag.sh against tags made in a scratch repository, one
# case per way a tag has been shown to pass that should not. The release
# workflow runs only on a tag push, so this is where its first step is tested.
#
# Needs git, ssh-keygen and gpg. Run from anywhere:
#
#   .github/scripts/verify-release-tag_test.sh

set -euo pipefail

check="$(cd "$(dirname "$0")" && pwd)/verify-release-tag.sh"
scratch="$(mktemp -d)"
cleanup() {
  local home
  for home in "$scratch"/gnupg-*; do
    gpgconf --homedir "$home" --kill all > /dev/null 2>&1 || true
  done
  rm -rf "$scratch"
}
trap cleanup EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=Maintainer GIT_AUTHOR_EMAIL=maintainer@example.org
export GIT_COMMITTER_NAME=Maintainer GIT_COMMITTER_EMAIL=maintainer@example.org

ssh-keygen -q -t ed25519 -N '' -C listed -f "$scratch/listed"
ssh-keygen -q -t ed25519 -N '' -C unlisted -f "$scratch/unlisted"
listed="maintainer@example.org namespaces=\"git\" $(cat "$scratch/listed.pub")"
unlisted="intruder@example.org namespaces=\"git\" $(cat "$scratch/unlisted.pub")"

# Makes an OpenPGP key without a passphrase in a GnuPG home of its own:
# pgp_key <name>
pgp_key() {
  local home="$scratch/gnupg-$1"
  mkdir -m 0700 "$home"
  GNUPGHOME="$home" gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
    --quick-gen-key "$1 <$1@example.org>" ed25519 sign never 2> /dev/null
}
pgp_key listed
pgp_key unlisted
pgp_listed="$(GNUPGHOME="$scratch/gnupg-listed" gpg --batch --armor --export)"
pgp_unlisted_private="$(GNUPGHOME="$scratch/gnupg-unlisted" gpg --batch --armor \
  --pinentry-mode loopback --passphrase '' --export-secret-keys)"

repo="$scratch/repo"
git init -q "$repo"
cd "$repo"
git commit -q --allow-empty -m 'first'
first="$(git rev-parse HEAD)"

# Signs a tag with the given key, an SSH key by its file name or an OpenPGP
# key as pgp:<name>: sign <key> <git tag arguments...>
sign() {
  local key="$1"
  shift
  case "$key" in
    pgp:*)
      GNUPGHOME="$scratch/gnupg-${key#pgp:}" git -c gpg.format=openpgp \
        -c user.signingKey="${key#pgp:}@example.org" tag -s "$@"
      ;;
    *)
      git -c gpg.format=ssh -c user.signingKey="$scratch/$key" tag -s "$@"
      ;;
  esac
}

failures=0

# run <name> <want: pass|fail> <tag> <sha> <SSH signers> <OpenPGP keys> [<pattern in output>]
run() {
  local name="$1" want="$2" tag="$3" sha="$4" signers="$5" pgp_keys="$6" pattern="${7:-}"
  local out got=pass
  out="$(GITHUB_REF_NAME="$tag" GITHUB_SHA="$sha" ALLOWED_SIGNERS="$signers" \
    ALLOWED_PGP_KEYS="$pgp_keys" GITHUB_OUTPUT="$scratch/output" "$check" 2>&1)" || got=fail
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
run 'a tag signed by a listed signer' pass v1.0.0 "$first" "$listed" '' 'is signed by a listed signer'
if ! grep -qx "object=$(git rev-parse refs/tags/v1.0.0)" "$scratch/output" ||
  ! grep -qx "commit=$first" "$scratch/output"; then
  echo "FAIL the verified tag object and commit are not in GITHUB_OUTPUT"
  failures=$((failures + 1))
fi
run 'the push event naming the tag object' pass v1.0.0 "$(git rev-parse refs/tags/v1.0.0)" "$listed" ''
run 'a signer list with a comment and blank lines' pass v1.0.0 "$first" \
  "$(printf '# release signers\n\n%s\n' "$listed")" ''

# 13.1: the keys come from outside the checkout, and none is a failure.
run 'no key lists' fail v1.0.0 "$first" '' '' 'RELEASE_ALLOWED_SIGNERS and RELEASE_ALLOWED_PGP_KEYS'
run 'blank key lists' fail v1.0.0 "$first" $'  \n' $'\n' 'RELEASE_ALLOWED_SIGNERS and RELEASE_ALLOWED_PGP_KEYS'

mkdir .github
printf '%s\n' "$unlisted" > .github/allowed_signers
git add .github/allowed_signers
git commit -q -m 'add my key'
added="$(git rev-parse HEAD)"
sign unlisted v1.0.1 -m 'clusterctl v1.0.1'
run 'a tagged commit listing its own signer' fail v1.0.1 "$added" "$listed" '' 'is not signed by a key'

git rm -q .github/allowed_signers
git commit -q -m 'drop the signer list'
dropped="$(git rev-parse HEAD)"
sign unlisted v1.0.5 -m 'clusterctl v1.0.5'
run 'a tagged commit deleting the signer list' fail v1.0.5 "$dropped" "$listed" '' 'is not signed by a key'

# 13.2: the name inside the signed object is the name pushed.
git update-ref refs/tags/v9.9.9 "$(git rev-parse refs/tags/v1.0.0)"
run 'a signed tag pushed under another name' fail v9.9.9 "$first" "$listed" '' "named 'v1.0.0' but was pushed as v9.9.9"

sign listed v1.1.0 -m 'clusterctl v1.1.0' -m 'tag v9.9.8' "$first"
git update-ref refs/tags/v9.9.8 "$(git rev-parse refs/tags/v1.1.0)"
run 'a tag message naming the pushed ref' fail v9.9.8 "$first" "$listed" '' "named 'v1.1.0'"

sign listed v1.2.0 -m 'clusterctl v1.2.0' "$first"
run 'a tag of another commit than the pushed one' fail v1.2.0 "$dropped" "$listed" '' "points at $first"

# 13.4: a signature is what git verifies, not text that looks like one.
git tag -a v1.3.0 "$first" -F - << 'MSG'
clusterctl v1.3.0

-----BEGIN SSH SIGNATURE-----
not a signature
-----END SSH SIGNATURE-----
MSG
run 'an unsigned tag quoting a signature' fail v1.3.0 "$first" "$listed" "$pgp_listed" 'is not signed by a key'

git tag -a v1.4.0 -m 'clusterctl v1.4.0' "$first"
run 'an unsigned annotated tag' fail v1.4.0 "$first" "$listed" "$pgp_listed" 'is not signed by a key'

git tag v1.5.0 "$first"
run 'a lightweight tag' fail v1.5.0 "$first" "$listed" "$pgp_listed" 'lightweight'

run 'a tag that does not exist' fail v1.6.0 "$first" "$listed" "$pgp_listed" 'not a tag'

# OpenPGP: a listed key passes, alone or next to SSH keys. Any other key does
# not, and neither does a signature in a format no key is listed for.
sign pgp:listed v1.7.0 -m 'clusterctl v1.7.0' "$first"
run 'a tag signed by a listed OpenPGP key' pass v1.7.0 "$first" '' "$pgp_listed" 'is signed by a listed signer'
run 'an OpenPGP tag with SSH keys listed too' pass v1.7.0 "$first" "$listed" "$pgp_listed"
run 'an OpenPGP tag with only SSH keys listed' fail v1.7.0 "$first" "$listed" '' 'is not signed by a key'
run 'an SSH tag with only OpenPGP keys listed' fail v1.0.0 "$first" '' "$pgp_listed" 'is not signed by a key'

sign pgp:unlisted v1.7.1 -m 'clusterctl v1.7.1' "$first"
run 'a tag signed by an unlisted OpenPGP key' fail v1.7.1 "$first" "$listed" "$pgp_listed" 'is not signed by a key'

# The OpenPGP list holds public keys and nothing else.
run 'an OpenPGP list holding a private key' fail v1.7.0 "$first" '' "$pgp_unlisted_private" 'private key'
run 'an OpenPGP list holding no key' fail v1.7.0 "$first" '' 'not a key' 'cannot import'

if [ "$failures" -ne 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
