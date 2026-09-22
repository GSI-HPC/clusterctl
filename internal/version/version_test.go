// SPDX-License-Identifier: LGPL-3.0-or-later

package version

import (
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
