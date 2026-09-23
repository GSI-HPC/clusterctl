// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package fileutil writes files the way a tool several administrators run at
// once has to: completely or not at all, and under a lock when the file is
// shared.
package fileutil

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// WriteAtomic writes data to path through a temporary file in the same
// directory, so a reader never sees a half written file and a failure leaves
// the previous content in place.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	return os.Rename(name, path)
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
func Lock(ctx context.Context, path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, "."+filepath.Base(path)+".lock"))

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if !ok {
		return nil, fmt.Errorf("another clusterctl is holding the lock on %s", path)
	}
	return func() { _ = lock.Unlock() }, nil
}

// EnsureDir creates a directory that only its owner can read, for state that
// holds host keys, control sockets and certificate pins.
func EnsureDir(path string) error {
	return os.MkdirAll(path, 0o700)
}

// CopyTo copies a reader into a new file with the given permission.
func CopyTo(path string, r io.Reader, perm os.FileMode) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return WriteAtomic(path, data, perm)
}
