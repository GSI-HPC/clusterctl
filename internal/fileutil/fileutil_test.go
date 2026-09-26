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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
	for range writers {
		wg.Go(func() {
			err := fileutil.Update(context.Background(), path, 0o600, func(current []byte) ([]byte, error) {
				return append(append([]byte{}, current...), 'x'), nil
			})
			if err != nil {
				errs <- err
			}
		})
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

// TestEnsureDirRefusesADirectoryOthersCanWrite keeps clusterctl from
// trusting a state directory someone else prepared. With HOME unset it was a
// fixed path in /tmp, created first by another user with mode 0777, and the
// ssh_config renamed into it ran its ProxyCommand as root.
func TestEnsureDirRefusesADirectoryOthersCanWrite(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.EnsureDir(open); !errors.Is(err, fileutil.ErrUntrusted) {
		t.Errorf("a directory with mode 0777: err = %v, want ErrUntrusted", err)
	}

	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.EnsureDir(link); !errors.Is(err, fileutil.ErrUntrusted) {
		t.Errorf("a link to a directory: err = %v, want ErrUntrusted", err)
	}

	if err := fileutil.EnsureDir("relative/state"); err == nil {
		t.Error("a relative path was accepted")
	}

	fresh := filepath.Join(base, "a", "b")
	if err := fileutil.EnsureDir(fresh); err != nil {
		t.Fatalf("a new directory: %v", err)
	}
	info, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
	if err := fileutil.EnsureDir(fresh); err != nil {
		t.Errorf("the directory it made: %v", err)
	}
}

func TestEnsureDirRefusesAnotherUsersDirectory(t *testing.T) {
	t.Parallel()
	uid, gid := otherIDs(t)

	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.EnsureDir(dir); !errors.Is(err, fileutil.ErrUntrusted) {
		t.Errorf("another user's directory: err = %v, want ErrUntrusted", err)
	}
}

func TestCheckTrusted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
		ok   bool
	}{
		{"private.yaml", 0o600, true},
		{"readable.yaml", 0o644, true},
		{"group-writable.yaml", 0o664, false},
		{"world-writable.yaml", 0o646, false},
	} {
		err := fileutil.CheckTrusted(write(tc.name, tc.mode))
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, fileutil.ErrUntrusted) {
			t.Errorf("%s: err = %v, want ErrUntrusted", tc.name, err)
		}
	}

	// A file in a sticky directory anyone can write, the way /tmp is, has
	// a trusted parent; one in a plain directory anyone can write has not.
	for _, tc := range []struct {
		mode os.FileMode
		ok   bool
	}{
		{0o755, true},
		{0o777 | fs.ModeSticky, true},
		{0o777, false},
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
		if !tc.ok && !errors.Is(err, fileutil.ErrUntrusted) {
			t.Errorf("parent with mode %v: err = %v, want ErrUntrusted", tc.mode, err)
		}
	}
}

func TestCheckTrustedRefusesAnotherUsersFile(t *testing.T) {
	t.Parallel()
	uid, gid := otherIDs(t)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.CheckTrusted(path); !errors.Is(err, fileutil.ErrUntrusted) {
		t.Errorf("another user's file: err = %v, want ErrUntrusted", err)
	}
	if err := os.Chown(path, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.CheckTrusted(path); err != nil {
		t.Errorf("root's file: %v", err)
	}
}

// A private file is created for this user alone and appended to; one that
// others can read or write is refused as it is found, and what is not a
// regular file is written as it is.
func TestAppendPrivate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "progress.jsonl")
	for _, line := range []string{"one\n", "two\n"} {
		f, err := fileutil.AppendPrivate(path)
		if err != nil {
			t.Fatalf("AppendPrivate: %v", err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("content = %q, want both lines", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("created with mode %v, want 0600", info.Mode().Perm())
	}

	for _, mode := range []os.FileMode{0o640, 0o604, 0o620, 0o644} {
		wide := filepath.Join(dir, "wide.jsonl")
		if err := os.WriteFile(wide, []byte("kept\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(wide, mode); err != nil {
			t.Fatal(err)
		}
		f, err := fileutil.AppendPrivate(wide)
		if err == nil {
			_ = f.Close()
			t.Errorf("a file with mode %v was opened", mode)
		} else if !strings.Contains(err.Error(), "chmod 600 "+wide) {
			t.Errorf("mode %v: err = %v, want it to say how to make it private", mode, err)
		}
		if data, _ := os.ReadFile(wide); string(data) != "kept\n" {
			t.Errorf("a refused file now holds %q", data)
		}
	}

	if _, err := os.Stat(os.DevNull); err == nil {
		f, err := fileutil.AppendPrivate(os.DevNull)
		if err != nil {
			t.Errorf("the null device, which anyone can write, was refused: %v", err)
		} else {
			_ = f.Close()
		}
	}
	if _, err := fileutil.AppendPrivate(filepath.Join(dir, "missing", "progress.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file in a directory that is not there: err = %v, want it not to exist", err)
	}
}

func TestAppendPrivateRefusesAnotherUsersFile(t *testing.T) {
	t.Parallel()
	uid, gid := otherIDs(t)

	path := filepath.Join(t.TempDir(), "progress.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if _, err := fileutil.AppendPrivate(path); !errors.Is(err, fileutil.ErrUntrusted) {
		t.Errorf("another user's file: err = %v, want ErrUntrusted", err)
	}
}

// A link another user made on the way to the log is not followed: not to
// a file of this user's it points at, which would be appended to, and not
// to where a file is not, which would be created. A link of this user's is
// followed, and so are the system's to a pipe or a terminal.
func TestAppendPrivateRefusesAnotherUsersLink(t *testing.T) {
	t.Parallel()
	uid, gid := otherIDs(t)

	dir := t.TempDir()
	mine := filepath.Join(dir, "config")
	if err := os.WriteFile(mine, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, target string }{
		{"to a file of mine", mine},
		{"to nothing", filepath.Join(dir, "profile.sh")},
	} {
		link := filepath.Join(dir, "planted-"+filepath.Base(tc.target))
		if err := os.Symlink(tc.target, link); err != nil {
			t.Fatal(err)
		}
		if err := os.Lchown(link, uid, gid); err != nil {
			t.Fatal(err)
		}
		if f, err := fileutil.AppendPrivate(link); !errors.Is(err, fileutil.ErrUntrusted) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("a link %s another user made: err = %v, want ErrUntrusted", tc.name, err)
		}
	}
	if data, _ := os.ReadFile(mine); string(data) != "kept\n" {
		t.Errorf("the file the link points at now holds %q", data)
	}
	if _, err := os.Lstat(filepath.Join(dir, "profile.sh")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file the dangling link names was created: %v", err)
	}

	own := filepath.Join(dir, "own-link")
	if err := os.Symlink(filepath.Join(dir, "progress.jsonl"), own); err != nil {
		t.Fatal(err)
	}
	f, err := fileutil.AppendPrivate(own)
	if err != nil {
		t.Fatalf("a link of this user's: %v", err)
	}
	_ = f.Close()
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
