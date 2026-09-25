// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package configtest_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
)

func TestCopyDirLeavesOutTheFilesNamedAndSubdirectories(t *testing.T) {
	from := configtest.WriteFiles(t, map[string]string{"site.yaml": "a", "cluster.yaml": "b", "secrets.yaml": "c"})
	if err := os.Mkdir(filepath.Join(from, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	dir := configtest.CopyDir(t, from, "secrets.yaml")
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range items {
		names = append(names, item.Name())
	}
	if want := []string{"cluster.yaml", "site.yaml"}; !slices.Equal(names, want) {
		t.Errorf("copied %q, want %q", names, want)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "cluster.yaml")); err != nil || string(data) != "b" {
		t.Errorf("cluster.yaml = %q (%v), want %q", data, err, "b")
	}
}
