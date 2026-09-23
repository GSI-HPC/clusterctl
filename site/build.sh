#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
# SPDX-License-Identifier: LGPL-3.0-or-later
#
# Builds the manual of every release and of main into one site, as it is
# published:
#
#   /         the latest release
#   /vX.Y/    the newest patch release of each minor line, the latest's too
#   /dev/     main, or whatever is checked out
#
#   site/build.sh [BASE_URL]
#
# BASE_URL is where the site will be served, http://localhost:8000/ unless
# given; the site is written to site/public. The releases are those published
# on GitHub, drafts and pre-releases left out, as gh lists them. RELEASES,
# a list of tags, names them instead. Their tags have to be fetched.
#
# A release's manual is built from its tag: its pages, its command reference
# and schemas from its own gendocs, its Hextra, and the Hugo release its
# site/hugo-version names. What every build is given is the list of versions,
# in a configuration file of its own, from which the version switcher and the
# banner are drawn. See doc/adr/0018-a-manual-for-every-release.md.

set -euo pipefail

base=${1:-http://localhost:8000/}
base=${base%/}/
host_and_path=${base#*://}
root=/${host_and_path#*/}

repo=$(git rev-parse --show-toplevel)
out=$repo/site/public
cache=${XDG_CACHE_HOME:-$HOME/.cache}/clusterctl-site
work=$(mktemp -d)
trap 'rm -rf "$work"; git -C "$repo" worktree prune' EXIT

# The templates that draw the switcher and the banner, below site/. A tag from
# before they existed is given these.
templates=(
  layouts/_partials/navbar-title.html
  layouts/_partials/custom/banner.html
  layouts/_partials/version-path.html
)

# hugo_version TREE prints the Hugo release the manual in TREE is built with.
hugo_version() {
  if [ -f "$1/site/hugo-version" ]; then
    sed -n 's/^\([0-9][0-9.]*\)[[:space:]]*$/\1/p' "$1/site/hugo-version"
  else
    # v0.1.0 predates site/hugo-version; its Pages workflow named this one.
    echo 0.152.0
  fi
}

# hugo_for VERSION prints the path of that Hugo release, extended edition:
# the one on the PATH if it is that release, else one fetched from GitHub and
# checked against the release's checksums.
hugo_for() {
  local version=$1 dir=$cache/hugo-$1 arch archive url
  if [ -z "$version" ]; then
    echo "build.sh: site/hugo-version names no Hugo release" >&2
    return 1
  fi
  if command -v hugo >/dev/null; then
    case $(hugo version) in
      "hugo v$version-"*"+extended"*) command -v hugo; return ;;
    esac
  fi
  if [ ! -x "$dir/hugo" ]; then
    case $(uname -sm) in
      "Linux x86_64") arch=linux-amd64 ;;
      "Linux aarch64" | "Linux arm64") arch=linux-arm64 ;;
      *)
        echo "build.sh: put Hugo $version extended on the PATH; there is no archive of it to fetch for $(uname -sm)" >&2
        return 1
        ;;
    esac
    archive=hugo_extended_${version}_$arch.tar.gz
    url=https://github.com/gohugoio/hugo/releases/download/v$version
    mkdir -p "$dir"
    curl -fsSL -o "$dir/$archive" "$url/$archive"
    curl -fsSL -o "$dir/checksums.txt" "$url/hugo_${version}_checksums.txt"
    (cd "$dir" && awk -v f="$archive" '$2 == f' checksums.txt | sha256sum --check --strict --quiet -)
    tar -xzf "$dir/$archive" -C "$dir" hugo
    rm "$dir/$archive" "$dir/checksums.txt"
  fi
  echo "$dir/hugo"
}

# versions_config VERSION PATH writes the configuration the build of VERSION
# at PATH is given, and prints its name.
versions_config() {
  local version=$1 path=$2 file
  file=$work/versions-$version-${path%/}.yaml
  cat >"$file" <<EOF
params:
  versions:
    root: "$root"
    path: "$path"
    version: "$version"
    latest: "$latest"
    list:
$list
EOF
  if [ "$version" != "$latest" ]; then
    # Dismissed until the next release. The key is namespaced because every
    # project site under gsi-hpc.github.io shares the browser's storage.
    cat >>"$file" <<EOF
  banner:
    key: "clusterctl-version-banner-$version-$latest"
EOF
  fi
  if [ -n "$path" ]; then
    # Search engines are to find each page once, at the root.
    cat >>"$file" <<EOF
cascade:
  - params:
      noindex: true
EOF
  fi
  echo "$file"
}

# build TREE VERSION PATH builds the manual in TREE into PATH below the root.
build() {
  local tree=$1 version=$2 path=$3 hugo
  local args=(--minify --baseURL "$base$path" --destination "$out/$path")
  hugo=$(hugo_for "$(hugo_version "$tree")")
  if [ -n "$latest" ]; then
    args+=(--config "hugo.yaml,$(versions_config "$version" "$path")")
  fi
  echo "build.sh: the manual of $version at $root$path" >&2
  (cd "$tree/site" && HUGO_ENVIRONMENT=production "$hugo" "${args[@]}")
}

# checkout TAG checks the tag out below the work directory and generates its
# pages there.
checkout() {
  local tag=$1 tree=$work/$1 file
  if ! git -C "$repo" rev-parse --quiet --verify "refs/tags/$tag^{commit}" >/dev/null; then
    echo "build.sh: the tag of release $tag is missing; git fetch --tags" >&2
    return 1
  fi
  git -C "$repo" worktree add --quiet --detach "$tree" "refs/tags/$tag"
  for file in "${templates[@]}"; do
    if [ ! -e "$tree/site/$file" ]; then
      mkdir -p "$(dirname "$tree/site/$file")"
      cp "$repo/site/$file" "$tree/site/$file"
    fi
  done
  (cd "$tree" && go run ./internal/tools/gendocs)
}

if [ -z "${RELEASES+set}" ]; then
  RELEASES=$(gh release list --repo "${GITHUB_REPOSITORY:-GSI-HPC/clusterctl}" \
    --exclude-drafts --exclude-pre-releases --limit 1000 --json tagName --jq '.[].tagName')
fi

# A patch release changes no interface, so each minor line has one manual:
# its newest release's. Newest line first.
semver='^v[0-9]+\.[0-9]+\.[0-9]+$'
# shellcheck disable=SC2086 # RELEASES is a list of words.
for tag in $(printf '%s\n' $RELEASES | { grep -vE "$semver" || true; }); do
  echo "build.sh: $tag is not a release version, vMAJOR.MINOR.PATCH, and has no manual" >&2
done
# shellcheck disable=SC2086
lines=$(printf '%s\n' $RELEASES | { grep -E "$semver" || true; } | sort -V |
  awk -F. '{ newest[$1 "." $2] = $0 } END { for (line in newest) print newest[line] }' |
  sort -rV)
latest=${lines%%$'\n'*}

list=""
for tag in $lines; do
  if [ "$tag" = "$latest" ]; then
    list+="      - {version: \"$tag\", path: \"\", note: latest}"$'\n'
  else
    list+="      - {version: \"$tag\", path: \"${tag%.*}/\"}"$'\n'
  fi
done
list+='      - {version: main, path: "dev/", note: unreleased}'

rm -rf "$out"

(cd "$repo" && go run ./internal/tools/gendocs)
if [ -z "$latest" ]; then
  # Before the first release, main's manual is the only one.
  build "$repo" main ""
  exit 0
fi
build "$repo" main dev/

for tag in $lines; do
  checkout "$tag"
  build "$work/$tag" "$tag" "${tag%.*}/"
  if [ "$tag" = "$latest" ]; then
    build "$work/$tag" "$tag" ""
  fi
done
