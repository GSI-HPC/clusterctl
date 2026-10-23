// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fileutil_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/fsutil"

	"github.com/GSI-HPC/clusterctl/internal/fileutil"
)

func TestWriteNewNeverReplacesAFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "file")
	if err := fileutil.WriteNew(path, []byte("one"), 0o644); err != nil {
		t.Fatalf("WriteNew failed: %v", err)
	}
	err := fileutil.WriteNew(path, []byte("two"), 0o644)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("writing over a file: err = %v, want fs.ErrExist", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "one"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func TestCacheKeepsAnAnswerForItsKeyAndTTL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	key := map[string]string{"host": "dhcp", "path": "/etc/dhcp/dhcpd.conf"}
	data := []byte("not UTF-8: \xff\n")
	fileutil.WriteCache(dir, key, data)

	if got, ok := fileutil.ReadCache(dir, key, time.Minute); !ok || string(got) != string(data) {
		t.Errorf("ReadCache = %q, %v; want the bytes written", got, ok)
	}
	if _, ok := fileutil.ReadCache(dir, map[string]string{"host": "dhcp"}, time.Minute); ok {
		t.Error("an entry was read back for another key")
	}
	if _, ok := fileutil.ReadCache(dir, key, time.Nanosecond); ok {
		t.Error("a stale entry was read back")
	}

	// An entry dated in the future would never age.
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("cache files = %q (%v), want one", files, err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	raw, _ := os.ReadFile(files[0])
	raw = regexp.MustCompile(`"at":"[^"]*"`).ReplaceAll(raw, []byte(`"at":"`+future+`"`))
	if err := os.WriteFile(files[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := fileutil.ReadCache(dir, key, time.Minute); ok {
		t.Error("an entry written in the future was trusted")
	}
}

// AppendSync creates a file the way WriteAtomic does, appends to one that
// exists, and takes write permission for anyone away as a rewrite would;
// Locked holds the lock Update takes, on the file a link points at.
func TestAppendSyncUnderLocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pins")
	link := filepath.Join(dir, "link")
	if err := os.Symlink("pins", link); err != nil {
		t.Fatal(err)
	}
	// The temporary directory may lie behind a link of its own, as
	// macOS's /var does, which Locked resolves too.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "pins")
	ctx := context.Background()
	for _, line := range []string{"a 1\n", "b 2\n"} {
		err := fileutil.Locked(ctx, link, func(resolved string) error {
			if resolved != want {
				t.Errorf("Locked handed %s, want the file the link points at, %s", resolved, want)
			}
			return fileutil.AppendSync(resolved, []byte(line), 0o600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a 1\nb 2\n" {
		t.Errorf("the file holds %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("a new file has mode %o, want 600", got)
	}

	if err := os.Chmod(path, 0o606); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.AppendSync(path, []byte("c 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o604 {
		t.Errorf("after an append the mode is %v, %v; want 604, writable by nobody else", info.Mode().Perm(), err)
	}
}

// A file in a sticky directory anyone can write, the way /tmp is, has a
// trusted parent, if the directory is this user's or root's; one in a plain
// directory anyone can write has not.
func TestCheckTrustedParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, tc := range []struct {
		mode os.FileMode
		ok   bool
	}{
		{0o755, true},
		{0o777 | fs.ModeSticky, true},
		{0o777, false},
		{0o775, false},
	} {
		sub, err := os.MkdirTemp(dir, "parent")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sub, tc.mode); err != nil {
			t.Fatal(err)
		}
		err = fileutil.CheckTrustedParent(filepath.Join(sub, "config.yaml"))
		if tc.ok && err != nil {
			t.Errorf("parent with mode %v: %v", tc.mode, err)
		}
		if !tc.ok && !errors.Is(err, fsutil.ErrUntrusted) {
			t.Errorf("parent with mode %v: err = %v, want ErrUntrusted", tc.mode, err)
		}
	}
}

// A sticky directory lets its owner remove what others put in it, so
// another user's is refused.
func TestCheckTrustedParentRefusesAnotherUsersStickyDirectory(t *testing.T) {
	t.Parallel()

	if os.Geteuid() != 0 {
		t.Skip("only root can give a directory to another user")
	}
	sub := filepath.Join(t.TempDir(), "sticky")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o777|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(sub, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.CheckTrustedParent(filepath.Join(sub, "config.yaml")); !errors.Is(err, fsutil.ErrUntrusted) {
		t.Errorf("another user's sticky directory: err = %v, want ErrUntrusted", err)
	}
}
