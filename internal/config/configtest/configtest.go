// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package configtest writes configuration directories for the tests of the
// packages that read one. Most start from the example configuration that
// ships with the manual, which keeps the example honest.
package configtest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// WriteFiles writes files into a new directory and returns it.
func WriteFiles(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// CopyDir copies the files of a directory, not its subdirectories, into a
// new directory, leaving out the files named, and returns it.
func CopyDir(t testing.TB, from string, leaveOut ...string) string {
	t.Helper()
	items, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, item := range items {
		if item.IsDir() || slices.Contains(leaveOut, item.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, item.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, item.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
