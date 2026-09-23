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
