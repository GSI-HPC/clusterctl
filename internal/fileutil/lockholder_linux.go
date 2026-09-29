// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build linux

package fileutil

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/GSI-HPC/clusterctl/internal/termtext"
)

// lockHolder names the process that holds the lock on the lock file name,
// as /proc/locks tells it, and returns "" where it does not: the holder may
// run on another host that shares the file, or in another PID namespace.
func lockHolder(name string) string {
	info, err := os.Stat(name)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	dev := uint64(st.Dev) //nolint:unconvert // uint32 on some architectures
	file := fmt.Sprintf("%02x:%02x:%d", unix.Major(dev), unix.Minor(dev), st.Ino)
	locks, err := os.ReadFile("/proc/locks")
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(locks)) {
		// "1: FLOCK  ADVISORY  WRITE 4242 fd:01:1834 0 EOF"; a process
		// waiting for the lock has "->" after the number, and holds
		// nothing.
		f := strings.Fields(line)
		if len(f) < 6 || f[1] != "FLOCK" || f[5] != file {
			continue
		}
		if pid, err := strconv.Atoi(f[4]); err == nil && pid > 0 {
			return describeProcess(f[4])
		}
	}
	return ""
}

// describeProcess names a process by its number, and by its name and user
// as far as /proc tells them.
func describeProcess(pid string) string {
	var about []string
	if comm, err := os.ReadFile("/proc/" + pid + "/comm"); err == nil {
		// A process chooses its own name, and it may be another user's.
		about = append(about, termtext.EscapeCell(strings.TrimSpace(string(comm))))
	}
	if info, err := os.Stat("/proc/" + pid); err == nil {
		if uid, _, ok := owner(info); ok {
			who := "uid " + strconv.Itoa(uid)
			if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
				who = u.Username
			}
			about = append(about, "run by "+who)
		}
	}
	if len(about) == 0 {
		return "process " + pid
	}
	return "process " + pid + " (" + strings.Join(about, ", ") + ")"
}
