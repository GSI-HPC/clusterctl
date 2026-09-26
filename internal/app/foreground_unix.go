// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build unix

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

// foreground returns what reports whether the process is in the foreground
// of the terminal f is, the job the shell hands the terminal to, and not
// one started with & or sent there by Ctrl-Z and bg. A terminal that cannot
// say is taken to be the process's.
func foreground(f *os.File) func() bool {
	return func() bool {
		group, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPGRP)
		return err != nil || group == unix.Getpgrp()
	}
}
