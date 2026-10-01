// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fileutil_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
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

// A lock held elsewhere said "locking ...: context deadline exceeded" after
// 30 seconds, and the message meant for it could never be shown. It now
// says which file is locked and, where the system tells, which process
// holds it: this one, here, through another descriptor. It stays a
// timeout, and the command's own end while it waits stays the command's.
func TestLockSaysWhoHoldsIt(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ssh-known-hosts")
	unlock, err := fileutil.Lock(context.Background(), path)
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}
	defer unlock()

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, err := fileutil.Lock(context.Background(), path)
		if err == nil {
			t.Fatal("a lock held elsewhere was taken")
		}
		if waited := time.Since(start); waited != 30*time.Second {
			t.Errorf("waited %v, want 30s", waited)
		}
		msg := err.Error()
		if !strings.Contains(msg, path) || strings.Contains(msg, "context deadline exceeded") {
			t.Errorf("error %q does not name the file, or names the deadline", msg)
		}
		if runtime.GOOS == "linux" && !strings.Contains(msg, fmt.Sprintf("process %d (", os.Getpid())) {
			t.Errorf("error %q does not name this process as the holder", msg)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %q is no longer a timeout", msg)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		go func() {
			time.Sleep(500 * time.Millisecond)
			cancel()
		}()
		if _, err := fileutil.Lock(ctx, path); !errors.Is(err, context.Canceled) {
			t.Errorf("interrupted while it waited: err = %v, want context.Canceled", err)
		}
	})
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

// linkedTarget lays out dir/c/target, the link dir/c/d/link to ../target,
// the linked directory dir/alias to c/d and the link dir/entry to
// alias/link, and returns the entry and the target. The kernel reads
// ../target against dir/c/d, where the link is, while dir/alias/../target
// spelled out is dir/target.
func linkedTarget(t *testing.T) (entry, target string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "c", "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(dir, "c", "target")
	if err := os.WriteFile(target, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, to := range map[string]string{
		filepath.Join(dir, "c", "d", "link"): filepath.Join("..", "target"),
		filepath.Join(dir, "alias"):          filepath.Join("c", "d"),
		filepath.Join(dir, "entry"):          filepath.Join("alias", "link"),
	} {
		if err := os.Symlink(to, link); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "entry"), target
}

// A relative link reached through a linked directory points where the
// system says, beside the directory the link is really in. The write went
// beside the name of the linked directory instead: it created a stray
// file there and left the file the links lead to as it was.
func TestAWriteFollowsARelativeLinkBehindALinkedDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		write func(path string) error
	}{
		{"WriteAtomic", func(path string) error { return fileutil.WriteAtomic(path, []byte("two\n"), 0o644) }},
		{"Update", func(path string) error {
			return fileutil.Update(context.Background(), path, 0o644, func(current []byte) ([]byte, error) {
				return append(current, "two\n"...), nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry, target := linkedTarget(t)
			if err := tc.write(entry); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			want := "two\n"
			if tc.name == "Update" {
				want = "one\ntwo\n"
			}
			if data, _ := os.ReadFile(target); string(data) != want {
				t.Errorf("the target holds %q, want %q", data, want)
			}
			stray := filepath.Join(filepath.Dir(entry), "target")
			if _, err := os.Lstat(stray); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a stray %s was written: %v", stray, err)
			}
			if info, err := os.Lstat(entry); err != nil || info.Mode()&fs.ModeSymlink == 0 {
				t.Errorf("the entry is no longer a link: %v", err)
			}
		})
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

// A link another user made is refused behind a linked directory as well.
// The walk read a relative link on the way beside the linked directory's
// name, found nothing there and stopped, and the open then went on through
// the link the system found, which nobody had checked.
func TestAppendPrivateRefusesAnotherUsersLinkBehindALinkedDirectory(t *testing.T) {
	t.Parallel()
	uid, gid := otherIDs(t)

	entry, target := linkedTarget(t)
	mine := filepath.Join(filepath.Dir(entry), "config")
	if err := os.WriteFile(mine, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mine, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(target, uid, gid); err != nil {
		t.Fatal(err)
	}
	if f, err := fileutil.AppendPrivate(entry); !errors.Is(err, fileutil.ErrUntrusted) {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("a link another user made behind a linked directory: err = %v, want ErrUntrusted", err)
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
