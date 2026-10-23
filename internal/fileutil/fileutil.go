// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package fileutil holds what clusterctl does with files beyond go-clikit's
// fsutil, which writes a file whole, locks it, updates it and checks it: a
// file created only when it is new, a file appended to under the lock of
// fsutil, a directory checked below which clusterctl is going to create
// one, and a cache of answers on disk.
package fileutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/GSI-HPC/go-clikit/fsutil"
)

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

// Locked runs fn holding the lock Update takes for a path, and hands it the
// path the lock belongs to, a link resolved, so that writers going through
// the link and around it take the same one.
func Locked(ctx context.Context, path string, fn func(path string) error) error {
	path, err := resolve(path)
	if err != nil {
		return err
	}
	unlock, err := fsutil.Lock(ctx, path, fsutil.DefaultLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(path)
}

// maxLinks bounds how many symbolic links resolve follows, as the system
// bounds the lookup of a path.
const maxLinks = 40

// resolve follows the symbolic links path is, to a file that may not exist
// yet, as fsutil does for the file it writes. A link that neither this user
// nor root owns is refused: whoever put it there would otherwise choose
// which file is written.
func resolve(path string) (string, error) {
	for range maxLinks {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && info.Mode()&fs.ModeSymlink == 0) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if err := trustedOwner(path, info); err != nil {
			return "", err
		}
		link, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			// Read against the directory the link is really in: behind a
			// linked directory, ../x is beside the directory it leads to.
			dir, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			link = filepath.Join(dir, link)
		}
		path = link
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}

// trustedOwner refuses a file neither this user nor root owns.
func trustedOwner(path string, info fs.FileInfo) error {
	if uid, ok := owner(info); ok && uid != 0 && uid != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, neither this user (uid %d) nor root: %w",
			path, uid, os.Geteuid(), fsutil.ErrUntrusted)
	}
	return nil
}

// AppendSync appends data to a file, creating it as fsutil.WriteAtomic does when
// there is none, and returns once the data has reached the disk. The caller
// holds the file's lock, from Locked.
//
// A file that is only ever appended to keeps every line it had, so a reader
// needs no lock as long as each append ends a line: whatever it reads is
// whole but for a last line that has not ended yet. Write permission for
// anyone is taken away, as a rewrite would.
func AppendSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return fsutil.WriteAtomic(path, data, perm)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err == nil && info.Mode().Perm()&0o002 != 0 {
		if err := f.Chmod(info.Mode().Perm() &^ 0o002); err != nil {
			return err
		}
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// CheckTrustedParent checks the directory holding path, whoever could
// replace path. A directory with the sticky bit set may be writable by
// anyone, the way /tmp is, because nobody can then replace or remove a file
// another user put there; its owner still can, so it has to be this user's
// or root's.
func CheckTrustedParent(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSticky == 0 {
		return fsutil.CheckTrusted(dir)
	}
	if err := trustedOwner(dir, info); err != nil {
		return err
	}
	return fsutil.CheckTrusted(filepath.Dir(dir))
}

// ReadCache returns what WriteCache stored under key in dir, if it was
// written less than ttl ago. The key is kept in the entry and compared, so
// an entry is only ever read back for the lookup that wrote it, and one
// written in the future, which would never age, is not trusted. Anything
// missing, stale or unreadable is a miss: the caller fetches again.
func ReadCache(dir string, key any, ttl time.Duration) ([]byte, bool) {
	path, id := cacheFile(dir, key)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, false
	}
	age := time.Since(entry.At)
	if entry.Key != id || len(entry.Data) == 0 || age < 0 || age > ttl {
		return nil, false
	}
	return entry.Data, true
}

// WriteCache stores data under key in dir for ReadCache. Nothing is kept of
// empty data, and a cache that cannot be written is not worth failing a
// command over, so nothing is reported.
func WriteCache(dir string, key any, data []byte) {
	path, id := cacheFile(dir, key)
	raw, err := json.Marshal(cacheEntry{Key: id, At: time.Now(), Data: data})
	if err == nil && len(data) > 0 {
		_ = fsutil.WriteAtomic(path, raw, 0o600)
	}
}

// cacheEntry is one cached answer. Data is kept as bytes, which JSON holds
// in base64, so that a file that is not UTF-8 comes back unchanged.
type cacheEntry struct {
	Key  string    `json:"key"`
	At   time.Time `json:"at"`
	Data []byte    `json:"data"`
}

// cacheFile names the entry of a key, the key being everything that decides
// the answer, and returns the key as the entry records it.
func cacheFile(dir string, key any) (path, id string) {
	raw, _ := json.Marshal(key)
	sum := sha256.Sum256(raw)
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), string(raw)
}
