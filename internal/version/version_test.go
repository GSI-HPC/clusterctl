// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestGetFallsBackToDevel(t *testing.T) {
	info := Get()
	if info.Version == "" {
		t.Error("Version is empty; a build without a tag must still report a version")
	}
	if info.GoVersion == "" || info.Platform == "" {
		t.Errorf("toolchain provenance is incomplete: %+v", info)
	}
}

func TestStringShortensTheRevision(t *testing.T) {
	info := Info{
		Version:   "v1.2.3",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		Date:      "2026-09-22T10:00:00Z",
		Dirty:     true,
		GoVersion: "go1.26.0",
		Platform:  "linux/amd64",
	}
	got := info.String()
	if !strings.Contains(got, "(0123456789ab-dirty)") {
		t.Errorf("String() = %q, want a 12 character revision marked dirty", got)
	}
	if !strings.HasPrefix(got, "v1.2.3 ") {
		t.Errorf("String() = %q, want the version first", got)
	}
}

func TestFromBuildInfo(t *testing.T) {
	const rev = "ebed4872343d0123456789abcdef0123456789ab"
	vcs := func(modified string) []debug.BuildSetting {
		return []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: rev},
			{Key: "vcs.time", Value: "2026-09-23T18:09:18Z"},
			{Key: "vcs.modified", Value: modified},
		}
	}
	tests := []struct {
		name     string
		main     string
		settings []debug.BuildSetting
		want     Info
	}{
		{
			// Go 1.24 and later stamp a pseudo-version from the checkout; the
			// binary is still an unreleased build and says so.
			name:     "a checkout build reports devel and its revision",
			main:     "v0.0.0-20260923180918-ebed4872343d",
			settings: vcs("false"),
			want:     Info{Version: "devel", Commit: rev, Date: "2026-09-23T18:09:18Z"},
		},
		{
			// A tag in a local checkout is not a signed release.
			name:     "a checkout of a tag reports devel",
			main:     "v1.4.0",
			settings: vcs("false"),
			want:     Info{Version: "devel", Commit: rev, Date: "2026-09-23T18:09:18Z"},
		},
		{
			name:     "a dirty checkout build is marked dirty",
			main:     "v0.0.0-20260923180918-ebed4872343d+dirty",
			settings: vcs("true"),
			want:     Info{Version: "devel", Commit: rev, Date: "2026-09-23T18:09:18Z", Dirty: true},
		},
		{
			// go install ...@v1.4.0 builds the module from the proxy, which
			// carries no VCS stamps; its version is vouched for by sum.golang.org.
			name: "go install of a release reports the module version",
			main: "v1.4.0",
			want: Info{Version: "v1.4.0"},
		},
		{
			name: "a build without any provenance reports devel",
			main: "(devel)",
			want: Info{Version: "devel"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bi := &debug.BuildInfo{Main: debug.Module{Path: "github.com/GSI-HPC/clusterctl", Version: tt.main}, Settings: tt.settings}
			got := fromBuildInfo(Info{}, bi)
			if got != tt.want {
				t.Errorf("fromBuildInfo() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFromBuildInfoPrefersTheInjectedValues(t *testing.T) {
	injected := Info{Version: "v1.4.0", Commit: "a1b2c3d4e5f6", Date: "2026-09-22T14:42:30Z"}
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.0.0-20260923180918-ebed4872343d"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "ebed4872343d"},
			{Key: "vcs.time", Value: "2026-09-23T18:09:18Z"},
		},
	}
	if got := fromBuildInfo(injected, bi); got != injected {
		t.Errorf("fromBuildInfo() = %+v, want the injected %+v", got, injected)
	}
}
