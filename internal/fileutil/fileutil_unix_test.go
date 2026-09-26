// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build unix

package fileutil_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/fileutil"
)

// A named pipe another user made is refused before it is opened, since
// whoever reads it would read every line; one of this user's is written,
// as the pipe a shell's process substitution hands on is.
func TestAppendPrivateRefusesAnotherUsersPipe(t *testing.T) {
	t.Parallel()
	uid, gid := otherIDs(t)

	fifo := filepath.Join(t.TempDir(), "progress.jsonl")
	if err := syscall.Mkfifo(fifo, 0o622); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(fifo, uid, gid); err != nil {
		t.Fatal(err)
	}
	if f, err := fileutil.AppendPrivate(fifo); !errors.Is(err, fileutil.ErrUntrusted) {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("another user's pipe: err = %v, want ErrUntrusted", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	path := "/dev/fd/" + strconv.Itoa(int(w.Fd()))
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no %s: %v", path, err)
	}
	f, err := fileutil.AppendPrivate(path)
	if err != nil {
		t.Fatalf("a pipe of this user's, as %s: %v", path, err)
	}
	_ = f.Close()
}

// A path that names one of the process's own descriptors, as
// --progress-log=/dev/stderr does with standard error redirected to a file,
// is written through that descriptor: the lines follow what the process
// wrote there, whatever mode the shell gave the file, and none is written
// over.
func TestAppendPrivateWritesThroughTheProcesssOwnDescriptor(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stderr")
	stderr, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderr.Close() }()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.WriteString("before\n"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"/dev/fd/", "/proc/self/fd/"} {
		name += strconv.Itoa(int(stderr.Fd()))
		if _, err := os.Stat(name); err != nil {
			continue
		}
		f, err := fileutil.AppendPrivate(name)
		if err != nil {
			t.Fatalf("AppendPrivate(%s): %v", name, err)
		}
		if _, err := f.WriteString("log " + name + "\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := stderr.WriteString("after " + name + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "before\nlog /dev/fd/"; !strings.HasPrefix(string(got), want) {
		t.Fatalf("the file reads %q, want it to start %q", got, want)
	}
	for _, line := range strings.SplitAfter(string(got), "\n") {
		if line != "" && !strings.HasSuffix(line, "\n") {
			t.Errorf("a line was written over: %q", got)
		}
	}
	if n := strings.Count(string(got), "\n"); n < 3 {
		t.Errorf("the file reads %q, want the lines of both writers in turn", got)
	}
}

// A descriptor open only for reading is refused, not written.
func TestAppendPrivateRefusesADescriptorOpenForReading(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	f, err := fileutil.AppendPrivate("/dev/fd/" + strconv.Itoa(int(r.Fd())))
	if err == nil {
		_ = f.Close()
		t.Fatal("a descriptor open for reading was taken")
	}
	if !strings.Contains(err.Error(), "not open for writing") {
		t.Errorf("err = %v, want it to say the descriptor is not open for writing", err)
	}
}
