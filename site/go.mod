// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later
//
// The documentation site is a Hugo module so that the theme is fetched
// through the same module proxy as the Go dependencies, with no Node and no
// package manager in the release path.

module github.com/GSI-HPC/clusterctl/site

go 1.24

require github.com/imfing/hextra v0.11.2
