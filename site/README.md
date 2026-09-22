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

Hugo **extended** 0.152 or newer. The manual is then at
<http://localhost:1313>.

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

## Publishing

`.github/workflows/pages.yml` builds and deploys on every push to the default
branch that touches the site, the design notes or the command tree, and on
every release. It needs GitHub Pages switched on for the repository, with
GitHub Actions as the source; `doc/release.md` says how.

Hextra fetches its search index library from a CDN while the site builds. An
air-gapped build needs the theme vendored and `params.search.enable` set to
false.
