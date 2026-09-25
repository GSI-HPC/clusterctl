// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY opens a pseudo terminal and returns the end a program sees as its
// terminal.
func openPTY(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no pseudo terminal: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Skipf("unlocking the pseudo terminal: %v", errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 {
		t.Skipf("naming the pseudo terminal: %v", errno)
	}
	tty, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("opening the pseudo terminal: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	return tty
}

// resize sets the size of a terminal, as a terminal emulator does when its
// window changes.
func resize(t *testing.T, tty *os.File, w, h int) {
	t.Helper()
	size := struct{ row, col, xpixel, ypixel uint16 }{row: uint16(h), col: uint16(w)}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size))); errno != 0 {
		t.Fatalf("resizing the terminal: %v", errno)
	}
}

// Only standard input was asked, so a command whose output went to a pipe
// could not tell that standard error was still a terminal, and one whose
// input was redirected could not tell that its output was. Output into a
// pipe is told from output into a file or the null device.
func TestEachStreamIsAskedWhetherItIsATerminal(t *testing.T) {
	tty := openPTY(t)
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	tests := []struct {
		name                    string
		in, out, errOut         *os.File
		isTTY, outIsTTY, errTTY bool
	}{
		{"no terminal anywhere", null, w, w, false, false, false},
		{"a terminal on standard input alone", tty, w, w, true, false, false},
		{"a terminal on standard output alone", null, tty, w, false, true, false},
		{"a terminal on standard error alone", null, w, tty, false, false, true},
		{"a terminal everywhere", tty, tty, tty, true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fileStreams(tc.in, tc.out, tc.errOut)
			if s.IsTTY != tc.isTTY || s.OutIsTTY != tc.outIsTTY || s.ErrIsTTY != tc.errTTY {
				t.Errorf("IsTTY, OutIsTTY, ErrIsTTY = %t, %t, %t; want %t, %t, %t",
					s.IsTTY, s.OutIsTTY, s.ErrIsTTY, tc.isTTY, tc.outIsTTY, tc.errTTY)
			}
			if got := s.Size != nil; got != tc.errTTY {
				t.Errorf("Size is set = %t, want %t: it measures the terminal on standard error", got, tc.errTTY)
			}
			if got, want := s.OutIsPipe, tc.out == w; got != want {
				t.Errorf("OutIsPipe = %t, want %t", got, want)
			}
		})
	}
	file, err := os.Create(t.TempDir() + "/out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for name, out := range map[string]*os.File{"a file": file, "the null device": null} {
		if fileStreams(null, out, tty).OutIsPipe {
			t.Errorf("standard output into %s is taken for a pipe", name)
		}
	}
}

// A display drawn on standard error has to fit the window it is in now, not
// the one the command started in.
func TestSizeFollowsTheTerminal(t *testing.T) {
	tty := openPTY(t)
	s := fileStreams(os.Stdin, os.Stdout, tty)
	if s.Size == nil {
		t.Fatal("Size is nil on a terminal")
	}
	for _, want := range [][2]int{{120, 40}, {80, 24}} {
		resize(t, tty, want[0], want[1])
		w, h, err := s.Size()
		if err != nil {
			t.Fatal(err)
		}
		if w != want[0] || h != want[1] {
			t.Errorf("Size() = %dx%d, want %dx%d", w, h, want[0], want[1])
		}
	}
}
