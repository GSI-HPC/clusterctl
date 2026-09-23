<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0016 — GSI holds the copyright, and every file says so

Status: accepted

## Context

[ADR 0008](0008-lgpl.md) chose the licence and put its identifier in every
file, but nothing in the tree said whose work was being licensed. The only
copyright lines were the Free Software Foundation's, and those cover the
licence texts, not the code. The manual's footer went further and named
someone: with no copyright line of its own it showed Hextra's default,
"© 2025 Hextra Project.", crediting the theme's authors with the site.

## Decision

The copyright holder is GSI Helmholtz Centre for Heavy Ion Research GmbH
<http://www.gsi.de>.

Every file that carries `SPDX-License-Identifier` carries an
`SPDX-FileCopyrightText` line naming the holder directly above it, in the
same comment style. That is the form the REUSE specification reads, and it
keeps the notice with the file when the file is copied out of the tree. The
README states the notice once for a reader, and the manual states it in its
footer and feed.

The year is 2026, when the work was first written. It is not moved forward
each January: that would touch every file, and a copyright does not depend
on it.

## Consequences

A new file starts with both lines. The pages under `site/content/` carry
neither, as before: they open with front matter, and the site's own notice is
the footer.
