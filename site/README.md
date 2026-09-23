<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# The clusterctl manual

The user manual, published at <https://gsi-hpc.github.io/clusterctl/>.

It is a [Hugo](https://gohugo.io) site using the
[Hextra](https://imfing.github.io/hextra/) theme. Both are Go programs fetched
through the module proxy, so building it needs Hugo and nothing else — no
Node, no package manager. See
[`../doc/adr/0009-hugo-for-the-manual.md`](../doc/adr/0009-hugo-for-the-manual.md)
for why.

## Building it

```console
$ go run ./internal/tools/gendocs   # from the repository root
$ cd site && hugo server
```

Hugo **extended**, the release [`hugo-version`](hugo-version) names. The
manual is then at <http://localhost:1313>.

Dependabot does not update the theme, because tidying a Hugo module as it does
removes the requirement. Update it here with
`hugo mod get -u github.com/imfing/hextra && hugo mod tidy`.

## What is written and what is generated

`content/docs/` is written by hand. Help text answers "what are the flags"; a
manual answers "how do I reinstall a rack", and generating one from the other
produces neither.

`content/reference/` and `static/schema/` are generated from the binary by
`internal/tools/gendocs` and are **not committed**, so they cannot drift from
what they describe. Both are gitignored and built in CI.

## Writing a page

- Front matter needs a `title` and a `weight`; the weight orders it in the
  sidebar.
- Show a command and its real output. A manual page that only describes flags
  is the command reference with extra steps.
- Where a behaviour exists for a reason — a confirmation, a quoted argument, a
  pinned certificate — say what the reason is. That is what a manual is for.
- `{{< callout type="warning" >}}` for anything that loses data;
  `type="info"` for a note worth not missing.

## Versions

The published site holds a manual per version: the latest release at the
root, the newest patch release of each minor line under `/vX.Y/`, and main
under `/dev/`. [`build.sh`](build.sh) builds them all into `public/`, each
release's from its own tag, with its own command reference, theme and Hugo:

```console
$ site/build.sh                     # from the repository root
$ python3 -m http.server -d site/public 8000
```

It lists the releases with `gh`; `RELEASES="v0.1.0 v0.2.0"` names them
instead. Their tags have to be fetched.

Each build is given the list of versions as an extra configuration file,
`params.versions`, and three templates draw from it:

- `layouts/_partials/navbar-title.html`, the theme's navbar title with the
  version switcher beside it. It replaces Hextra's, so check it when the theme
  is upgraded.
- `layouts/_partials/custom/banner.html`, the banner over every manual but the
  latest release's.
- `layouts/_partials/version-path.html`, the path of a page below its
  version's base, from which both link to the same page elsewhere.

`hugo server` is given no versions, and shows neither.
[`../doc/adr/0018-a-manual-for-every-release.md`](../doc/adr/0018-a-manual-for-every-release.md)
says why it is built this way.

## Publishing

`.github/workflows/pages.yml` runs `build.sh` and deploys the result on every
push to the default branch that touches the site, the design notes or the
command tree, and after every release. It needs GitHub Pages switched on for
the repository, with GitHub Actions as the source; `doc/release.md` says how.

Hextra fetches its search index library from a CDN while the site builds. An
air-gapped build needs the theme vendored and `params.search.enable` set to
false.
