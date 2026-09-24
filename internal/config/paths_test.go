// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config_test

import (
	"path/filepath"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
)

// TestStateAndCacheDirsNeedAHome is report section 9.4: without HOME the
// state and cache directories were fixed paths in /tmp, shared by every user
// of the host. A relative XDG value is ignored, as the specification says.
func TestStateAndCacheDirsNeedAHome(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct {
		name, home, xdg string
		want            string // empty when there is no directory
	}{
		{"xdg", "", base, filepath.Join(base, "clusterctl")},
		{"home", "/home/alice", "", "/home/alice/%s/clusterctl"},
		{"relative xdg", "/home/alice", "rel", "/home/alice/%s/clusterctl"},
		{"nothing", "", "", ""},
		{"relative xdg only", "", "rel", ""},
		{"relative home", "rel", "", ""},
	} {
		for _, dir := range []struct {
			variable, fallback string
			find               func() (string, error)
		}{
			{"XDG_STATE_HOME", ".local/state", config.StateDir},
			{"XDG_CACHE_HOME", ".cache", config.CacheDir},
		} {
			t.Run(tc.name+"/"+dir.variable, func(t *testing.T) {
				t.Setenv("HOME", tc.home)
				t.Setenv(dir.variable, tc.xdg)
				got, err := dir.find()
				want := tc.want
				if want != "" && tc.xdg != base {
					want = filepath.Join(tc.home, dir.fallback, "clusterctl")
				}
				switch {
				case want == "" && err == nil:
					t.Errorf("got %q, want an error", got)
				case want != "" && err != nil:
					t.Errorf("unexpected error: %v", err)
				case got != want:
					t.Errorf("got %q, want %q", got, want)
				}
			})
		}
	}
}
