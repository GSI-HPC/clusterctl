// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package fileutil writes files the way a tool several administrators run at
// once has to: completely or not at all, and under a lock when the file is
// shared.
package fileutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// ErrUntrusted is matched by errors.Is for every file or directory refused
// because someone other than this user or root could have written it.
var ErrUntrusted = errors.New("written by someone else")

// untrustedError says which file was refused and why, and is ErrUntrusted.
type untrustedError struct{ msg string }

func (e *untrustedError) Error() string        { return e.msg }
func (e *untrustedError) Is(target error) bool { return target == ErrUntrusted }

func untrusted(format string, args ...any) error {
	return &untrustedError{fmt.Sprintf(format, args...)}
}

// maxLinks bounds how many symbolic links a write follows, the way the
// kernel bounds a path lookup.
const maxLinks = 40

// WriteAtomic writes data to path through a temporary file in the same
// directory, so a reader never sees a half written file and a failure leaves
// the previous content in place.
//
// A file that exists already keeps its mode and group, and its owner when
// root writes it, so that a file several administrators share stays
// writable by all of them; perm applies only to a new file. Write
// permission for anyone is never kept. A symbolic link is written through
// rather than replaced, as long as this user or root made it.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	path, err := resolve(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	uid, gid := -1, -1
	switch info, err := os.Stat(path); {
	case err == nil:
		perm = info.Mode().Perm() &^ 0o002
		if u, g, ok := owner(info); ok {
			gid = g
			if os.Geteuid() == 0 {
				uid = u
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		// Both are best effort: on the happy path the file has already
		// been closed and renamed away.
		_ = tmp.Close()
		_ = os.Remove(name)
	}()

	if gid >= 0 {
		if err := keepOwner(tmp, uid, gid); err != nil {
			return fmt.Errorf("keeping the owner of %s: %w", path, err)
		}
	}
	// After the change of owner, which may clear the setgid bit.
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// The rename is only durable once the content has reached the disk.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// And the rename itself only once the directory has.
	return syncDir(dir)
}

// keepOwner gives a new file the group, and the owner when uid is not -1, of
// the file it is going to replace, unless it has them already.
func keepOwner(f *os.File, uid, gid int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	u, g, ok := owner(info)
	if !ok || (g == gid && (uid < 0 || u == uid)) {
		return nil
	}
	return f.Chown(uid, gid)
}

// syncDir makes a change to the entries of a directory durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// resolve follows the symbolic links path is, so that a write through a link
// changes the file it points at. A link that neither this user nor root owns
// is refused: whoever put it there would otherwise choose which file is
// written.
func resolve(path string) (string, error) {
	for range maxLinks {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return path, nil
		}
		if err := trustedOwner(path, info); err != nil {
			return "", err
		}
		link, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(path), link)
		}
		path = link
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}

// WriteNew writes data to a file that does not exist yet, and never to one
// that does. The file is created exclusively, so one that appeared since it
// was last looked for is left alone and reported with an error errors.Is
// matches to fs.ErrExist. A write that fails takes the new file away again.
func WriteNew(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// Update reads a file, hands its content to change and writes back whatever
// comes out, holding an exclusive lock for the whole cycle. A file that does
// not exist yet is presented as empty.
//
// The known_hosts file is shared and versioned, and ssh appends to it without
// locking, so every write clusterctl makes to it goes through here.
func Update(ctx context.Context, path string, perm os.FileMode, change func([]byte) ([]byte, error)) error {
	// The lock belongs to the file a link points at, so that writers
	// going through the link and around it take the same one.
	path, err := resolve(path)
	if err != nil {
		return err
	}
	unlock, err := Lock(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()

	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	next, err := change(current)
	if err != nil {
		return err
	}
	return WriteAtomic(path, next, perm)
}

// Lock takes an exclusive lock for a path and returns the function that
// releases it. The lock lives in a sibling file, so that the locked file can
// still be replaced atomically.
//
// The lock file is readable, which is all taking the lock needs, by whoever
// can write the file: its group when the file, or while there is none yet
// its directory, is writable by the group. A lock only its first user could
// open would lock every other administrator out of a shared file for good.
func Lock(ctx context.Context, path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	perm, gid := lockMode(path)
	name := filepath.Join(dir, "."+filepath.Base(path)+".lock")
	lock := flock.New(name, flock.SetPermissions(perm))

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if !ok {
		return nil, fmt.Errorf("another clusterctl is holding the lock on %s", path)
	}
	shareLock(name, perm, gid)
	return func() { _ = lock.Unlock() }, nil
}

// lockMode returns the mode and group a lock for path is created with: the
// group's read and write permission is that of the file, or of its directory
// when there is no file yet.
func lockMode(path string) (os.FileMode, int) {
	info, err := os.Stat(path)
	if err != nil {
		if info, err = os.Stat(filepath.Dir(path)); err != nil {
			return 0o600, -1
		}
	}
	gid := -1
	if _, g, ok := owner(info); ok {
		gid = g
	}
	perm := os.FileMode(0o600)
	if info.Mode().Perm()&0o020 != 0 {
		perm |= 0o060
	}
	return perm, gid
}

// shareLock gives a lock file this user created the mode and group it was
// meant to have, which the umask and a directory without the setgid bit
// take away. It is best effort: the lock is held either way, and only
// another administrator's next run can be stopped by it.
func shareLock(name string, perm os.FileMode, gid int) {
	info, err := os.Stat(name)
	if err != nil {
		return
	}
	u, g, ok := owner(info)
	if !ok || u != os.Geteuid() {
		return
	}
	if gid >= 0 && g != gid {
		_ = os.Chown(name, -1, gid)
	}
	if info.Mode().Perm() != perm {
		_ = os.Chmod(name, perm)
	}
}

// EnsureDir creates a directory that only its owner can write, for state
// that holds host keys, control sockets and certificate pins, and checks the
// one that is there already. It has to be a directory, not a link to one,
// owned by this user and writable by nobody else: whoever else can write it
// can replace the ssh configuration clusterctl hands to ssh.
func EnsureDir(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not an absolute path", path)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return untrusted("%s is a symbolic link, not a directory", path)
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", path)
	}
	if uid, _, ok := owner(info); ok && uid != os.Geteuid() {
		return untrusted("%s is owned by uid %d, not by this user (uid %d)",
			path, uid, os.Geteuid())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return untrusted("%s can be written by others (mode %v); run chmod go-w %s",
			path, info.Mode().Perm(), path)
	}
	return nil
}

// CheckTrusted refuses a file or directory that someone other than this
// user or root could have written: one another user owns, or one its group
// or anyone can write. Configuration names the programs clusterctl runs, so
// it is held to what OpenSSH holds ~/.ssh/config to. A link is followed, and
// what it points at is checked.
func CheckTrusted(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := trustedOwner(path, info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return untrusted("%s can be written by others than its owner (mode %v); run chmod go-w %s",
			path, info.Mode().Perm(), path)
	}
	return nil
}

// CheckTrustedParent checks the directory holding path, whoever could
// replace path. A directory with the sticky bit set may be writable by
// anyone, the way /tmp is, because nobody can then replace or remove a file
// another user put there.
func CheckTrustedParent(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSticky != 0 {
		return trustedOwner(dir, info)
	}
	return CheckTrusted(dir)
}

// trustedOwner refuses a file neither this user nor root owns.
func trustedOwner(path string, info fs.FileInfo) error {
	uid, _, ok := owner(info)
	if !ok || uid == 0 || uid == os.Geteuid() {
		return nil
	}
	return untrusted("%s is owned by uid %d, neither this user (uid %d) nor root",
		path, uid, os.Geteuid())
}
