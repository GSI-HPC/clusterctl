// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build !unix

package fileutil

import "io/fs"

// owner is not known where files have no Unix owner.
func owner(fs.FileInfo) (uid, gid int, ok bool) {
	return 0, 0, false
}
