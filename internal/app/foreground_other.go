// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build !unix

package app

import "os"

// foreground knows no jobs where there are no process groups: the process
// is taken to be in the foreground.
func foreground(*os.File) func() bool { return nil }
