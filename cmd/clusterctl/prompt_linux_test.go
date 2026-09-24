// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPTY opens a pseudo terminal and returns its two ends.
func openPTY(t *testing.T) (master, tty *os.File) {
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
	tty, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("opening the pseudo terminal: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	return master, tty
}

// screen collects what the process writes to its terminal.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestCtrlCAtAPromptExits130 checks that Ctrl-C typed at a prompt ends the
// command. Three Ctrl-C at "Continue? [y/N]" were ignored, and after a
// password prompt was answered the command went on to report the BMC as
// failed.
func TestCtrlCAtAPromptExits130(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the process on a pseudo terminal")
	}
	tests := []struct {
		name   string
		args   []string
		prompt string
	}{
		// exec asks before it contacts anything; slurm node drain asks
		// Slurm first, which the hanging ssh here would never answer.
		{"confirmation", []string{"exec", "--confirm", "-n", "exe0001", "--", "uptime"}, "Continue? [y/N]"},
		// The pdu credential asks on the terminal.
		{"password", []string{"--set", "bmc.credential=pdu", "bmc", "status", "-n", "exe0001"}, "Password for admin@pdu:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binary, started := fakeSSH(t)
			master, tty := openPTY(t)
			cmd := child(t, "main", append([]string{
				"--config", "../../examples/site", "--set", "ssh.binary=" + binary,
			}, tc.args...)...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
			// The terminal is the process's own, as it is for a shell's
			// foreground job, so that Ctrl-C reaches it as SIGINT.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var out screen
			go func() { _, _ = out.ReadFrom(master) }()

			deadline := time.Now().Add(10 * time.Second)
			for !strings.Contains(out.String(), tc.prompt) {
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatalf("the prompt %q never came; the terminal shows:\n%s", tc.prompt, out.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			if _, err := master.Write([]byte{0x03}); err != nil {
				t.Fatal(err)
			}

			_ = waitFor(t, cmd, 5*time.Second)
			if code := cmd.ProcessState.ExitCode(); code != 130 {
				t.Errorf("exit code = %d, want 130; the terminal shows:\n%s", code, out.String())
			}
			if _, err := os.Stat(started); err == nil {
				t.Error("ssh was started after Ctrl-C")
			}
			if !echoes(t, tty) {
				t.Error("the terminal was left without echo")
			}
		})
	}
}

// ReadFrom copies everything from r until it fails, which it does when the
// terminal's last user has gone.
func (s *screen) ReadFrom(r *os.File) (int64, error) {
	var total int64
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = s.Write(buf[:n])
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}

// echoes reports whether a terminal echoes what is typed.
func echoes(t *testing.T, tty *os.File) bool {
	t.Helper()
	var state syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&state))); errno != 0 {
		t.Fatalf("reading the terminal state: %v", errno)
	}
	return state.Lflag&syscall.ECHO != 0
}
