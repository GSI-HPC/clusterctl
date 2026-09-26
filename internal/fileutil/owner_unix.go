// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build unix

package fileutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// owner returns the user and group that own a file.
func owner(info fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// noFollow opens a path only when it is not a symbolic link.
const noFollow = syscall.O_NOFOLLOW

// ownDescriptor returns a descriptor of its own for the open file path
// names when that is one of this process's, /dev/stdout, /dev/stderr,
// /dev/fd/N or /proc/self/fd/N, and ok false for any other path. The
// descriptor shares the file's offset with the one the path names, as a
// shell's 2>&1 does, so that what either writes follows what the other
// wrote. One not open for writing is refused.
func ownDescriptor(path string) (f *os.File, ok bool, err error) {
	fd := -1
	switch p := filepath.Clean(path); p {
	case "/dev/stdout":
		fd = 1
	case "/dev/stderr":
		fd = 2
	default:
		for _, dir := range []string{"/dev/fd/", "/proc/self/fd/"} {
			if n, found := strings.CutPrefix(p, dir); found {
				if v, err := strconv.Atoi(n); err == nil && v >= 0 {
					fd = v
				}
			}
		}
	}
	if fd < 0 {
		return nil, false, nil
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, true, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	if flags&unix.O_ACCMODE == unix.O_RDONLY {
		return nil, true, &fs.PathError{Op: "open", Path: path, Err: errors.New("not open for writing")}
	}
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, true, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(dup), path), true, nil
}
