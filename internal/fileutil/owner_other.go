// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build !unix

package fileutil

import (
	"io/fs"
	"os"
)

// owner is not known where files have no Unix owner.
func owner(fs.FileInfo) (uid, gid int, ok bool) {
	return 0, 0, false
}

// noFollow is not there to be had.
const noFollow = 0

// ownDescriptor finds no descriptor of this process's by its path: a path
// is a file to open.
func ownDescriptor(string) (f *os.File, ok bool, err error) {
	return nil, false, nil
}
