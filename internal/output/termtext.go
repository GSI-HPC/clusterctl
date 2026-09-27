// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import "github.com/GSI-HPC/clusterctl/internal/termtext"

// EscapeText is termtext.EscapeText: untrusted text made safe for a
// terminal, its lines kept.
func EscapeText(s string) string { return termtext.EscapeText(s) }

// EscapeCell is termtext.EscapeCell: untrusted text made safe for one line
// of a terminal.
func EscapeCell(s string) string { return termtext.EscapeCell(s) }
