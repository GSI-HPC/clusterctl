// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fileutil_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/fileutil"
)

func TestWriteAtomic(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sub", "file")
	if err := fileutil.WriteAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("WriteAtomic failed: %v", err)
	}
	if err := fileutil.WriteAtomic(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("WriteAtomic failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "two"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}

	// No temporary file is left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

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

// TestUpdateSerialises checks that concurrent updates do not lose writes,
// which is what an unlocked append to a shared known_hosts file does.
func TestUpdateSerialises(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "known_hosts")
	const writers = 8

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := fileutil.Update(context.Background(), path, 0o600, func(current []byte) ([]byte, error) {
				return append(append([]byte{}, current...), 'x'), nil
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Update failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(data); got != writers {
		t.Errorf("the file holds %d bytes, want %d; a write was lost", got, writers)
	}
}

func TestUpdateReportsChangeErrors(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "file")
	want := os.ErrInvalid
	err := fileutil.Update(context.Background(), path, 0o600, func([]byte) ([]byte, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Errorf("Update returned %v, want %v", err, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a failed change must not create the file")
	}
}

// otherIDs returns a user and a group that are not this process's, for the
// tests that need a file someone else owns. Only root can make one.
func otherIDs(t *testing.T) (uid, gid int) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("only root can give a file to another user")
	}
	return 65534, 65534
}

// TestLockIsSharedWithTheGroup keeps the first administrator to write a
// shared file from owning a lock nobody else can open. The host key file in
// a setgid group directory was locked with mode 0600, and every later write
// by another member of the group failed with permission denied.
func TestLockIsSharedWithTheGroup(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o2775); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ssh-known-hosts")
	err := fileutil.Update(context.Background(), path, 0o644, func([]byte) ([]byte, error) {
		return []byte("x\n"), nil
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, ".ssh-known-hosts.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o060 != 0o060 {
		t.Errorf("lock mode = %v, want the group to read and write it", got)
	}

	// A file its group cannot write keeps a private lock.
	private := filepath.Join(t.TempDir(), "pins")
	unlock, err := fileutil.Lock(context.Background(), private)
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}
	unlock()
	info, err = os.Stat(filepath.Join(filepath.Dir(private), ".pins.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("private lock mode = %v, want %v", got, want)
	}
}

// TestWriteAtomicKeepsModeAndGroup keeps a shared file writable by the
// group it is shared with after one of its members rewrote it.
func TestWriteAtomicKeepsModeAndGroup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ssh-known-hosts")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.WriteAtomic(path, []byte("two\n"), 0o644); err != nil {
		t.Fatalf("WriteAtomic failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o664); got != want {
		t.Errorf("mode = %v, want %v, the mode the file had", got, want)
	}

	// Write permission for anyone is not carried over.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.WriteAtomic(path, []byte("three\n"), 0o644); err != nil {
		t.Fatalf("WriteAtomic failed: %v", err)
	}
	if info, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o664); got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
}

func TestWriteAtomicKeepsOwnerAndGroupAsRoot(t *testing.T) {
	t.Parallel()
	uid, gid := otherIDs(t)

	path := filepath.Join(t.TempDir(), "ssh-known-hosts")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.WriteAtomic(path, []byte("two\n"), 0o644); err != nil {
		t.Fatalf("WriteAtomic failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if int(st.Uid) != uid || int(st.Gid) != gid {
		t.Errorf("owner = %d:%d, want %d:%d, the owner the file had", st.Uid, st.Gid, uid, gid)
	}
}

// TestWriteAtomicWritesThroughALink keeps a link to a shared file a link,
// rather than turning it into a private copy.
func TestWriteAtomicWritesThroughALink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", link); err != nil {
		t.Fatal(err)
	}
	err := fileutil.Update(context.Background(), link, 0o644, func(current []byte) ([]byte, error) {
		return append(current, "two\n"...), nil
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Error("the link was replaced by a file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "one\ntwo\n"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

// TestWriteAtomicRefusesAnotherUsersLink keeps someone who can write a shared
// directory from redirecting root's next write to a file of their choice.
func TestWriteAtomicRefusesAnotherUsersLink(t *testing.T) {
	t.Parallel()
	uid, gid := otherIDs(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	link := filepath.Join(dir, "ssh-known-hosts")
	if err := os.WriteFile(target, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(link, uid, gid); err != nil {
		t.Fatal(err)
	}
	err := fileutil.WriteAtomic(link, []byte("owned\n"), 0o644)
	if !errors.Is(err, fileutil.ErrUntrusted) {
		t.Fatalf("WriteAtomic through another user's link: err = %v, want ErrUntrusted", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "keep\n" {
		t.Errorf("the link's target was written: %q", data)
	}
}
