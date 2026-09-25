<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Decisions

One file per decision, in the order they were taken. A record says what the
situation was, what was decided, and what that costs — not what would have been
nice.

A decision is superseded by a later record, never edited.

| | Decision | Status |
| --- | --- | --- |
| [0001](0001-go.md) | Go as the implementation language | accepted |
| [0002](0002-own-nodeset-engine.md) | Port the node set engine rather than depend on one | accepted |
| [0003](0003-layered-yaml-configuration.md) | Layered YAML documents with provenance | accepted |
| [0004](0004-drive-openssh.md) | Drive OpenSSH rather than speak SSH | accepted |
| [0005](0005-own-redfish-client.md) | Write the Redfish client rather than take gofish | accepted |
| [0006](0006-clean-break.md) | No compatibility layer for the shell toolkit | accepted |
| [0007](0007-version-from-signed-tags.md) | The version lives only in a signed tag | accepted |
| [0008](0008-lgpl.md) | LGPL-3.0-or-later | accepted |
| [0009](0009-hugo-for-the-manual.md) | Hugo and Hextra for the manual | accepted |
| [0010](0010-public-nodeset-package.md) | Publish `nodeset`, keep the rest internal | accepted |
| [0011](0011-installable-from-release-assets.md) | Release assets are consumable by a version manager | accepted |
| [0012](0012-dependabot.md) | Dependabot proposes the dependency updates | accepted |
| [0013](0013-sops-secret-documents.md) | Secrets live in sops encrypted Secret documents | superseded in part by [0019](0019-decrypt-with-the-sops-command.md) |
| [0014](0014-mcp-plan-and-apply.md) | An MCP server of curated tools, where a person answers the gate | accepted |
| [0015](0015-release-notes-in-the-tag.md) | Release notes are written in the signed tag | accepted |
| [0016](0016-copyright-holder.md) | GSI holds the copyright, and every file says so | accepted |
| [0017](0017-config-init-into-an-empty-directory.md) | `config init` writes only into an empty directory, where it is read | accepted |
| [0018](0018-a-manual-for-every-release.md) | A manual for every release, the latest at the root | accepted |
| [0019](0019-decrypt-with-the-sops-command.md) | Decrypt Secret documents with the sops command | accepted |
| [0020](0020-one-exit-code-rule-for-many-hosts.md) | One exit code rule for a command on many hosts | accepted |
