// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
)

// A long running process reads its configuration through a cache, which
// hands out what it read while every file is still the file read, and reads
// them again after an edit, another list of files or a file it could not
// read: an edit to the inventory or the protected hosts is seen by the next
// call, as it was when every call read everything.
func TestTheCacheReadsAFileAgainOnceItChanged(t *testing.T) {
	dir := configtest.CopyDir(t, exampleDir)
	cache := &app.Cache{}
	state := t.TempDir()
	build := func(files ...string) (*app.App, error) {
		return app.New(app.WithCache(context.Background(), cache), app.Streams{
			In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{},
			StateDir: filepath.Join(state, "state"), CacheDir: filepath.Join(state, "cache"),
		}, app.Options{ConfigFiles: files, Env: func(string) string { return "" }})
	}
	mustBuild := func(files ...string) *app.App {
		t.Helper()
		a, err := build(files...)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	first := mustBuild(dir)
	second := mustBuild(dir)
	if second.Resolved.Bundle != first.Resolved.Bundle || second.Inventory != first.Inventory {
		t.Error("the configuration was read again although no file changed")
	}
	if second.Resolved == first.Resolved {
		t.Error("the merged configuration was kept; it has to be resolved for every call")
	}

	inventory := filepath.Join(dir, "inventory.yaml")
	data, err := os.ReadFile(inventory)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "rack: R03\n", "rack: R09\n", 1)
	if edited == string(data) {
		t.Fatal("the example inventory has no node in rack R03 to move")
	}
	if err := os.WriteFile(inventory, []byte(edited+"# moved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third := mustBuild(dir)
	if third.Resolved.Bundle == second.Resolved.Bundle {
		t.Fatal("an edited inventory was not read again")
	}
	if got := third.Inventory.InRack("R09").String(); got != "sub[0001-0002]" {
		t.Errorf("rack R09 holds %q after the edit, want sub[0001-0002]", got)
	}

	// Another list of files is read on its own.
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var fewer []string
	for _, f := range files {
		if base := filepath.Base(f); base != "workstation.yaml" && !strings.HasPrefix(base, ".") {
			fewer = append(fewer, f)
		}
	}
	if fourth := mustBuild(fewer...); fourth.Resolved.Bundle == third.Resolved.Bundle {
		t.Error("another list of files was answered from the cache")
	}

	// A file that cannot be read is reported, and nothing stale is kept.
	if err := os.WriteFile(inventory, []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := build(dir); err == nil {
		t.Error("a broken inventory was not reported")
	}
	if err := os.WriteFile(inventory, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustBuild(dir).Inventory.InRack("R03").String(); got != "sub[0001-0002]" {
		t.Errorf("rack R03 holds %q once the inventory was put back, want sub[0001-0002]", got)
	}
}
