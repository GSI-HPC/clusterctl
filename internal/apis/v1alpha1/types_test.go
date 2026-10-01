// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package v1alpha1_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
)

func TestCheckTypeMeta(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		meta    v1alpha1.TypeMeta
		wantErr string
	}{
		{"a known kind passes", v1alpha1.TypeMeta{APIVersion: v1alpha1.GroupVersion, Kind: v1alpha1.KindSite}, ""},
		{"a missing apiVersion is reported", v1alpha1.TypeMeta{Kind: v1alpha1.KindSite}, "apiVersion is missing"},
		{"a later apiVersion is reported", v1alpha1.TypeMeta{APIVersion: "clusterctl/v2", Kind: v1alpha1.KindSite}, "unsupported apiVersion"},
		{"a missing kind is reported", v1alpha1.TypeMeta{APIVersion: v1alpha1.GroupVersion}, "kind is missing"},
		{"an unknown kind is reported", v1alpha1.TypeMeta{APIVersion: v1alpha1.GroupVersion, Kind: "Node"}, "unknown kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := v1alpha1.CheckTypeMeta(tc.meta)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("CheckTypeMeta returned %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("CheckTypeMeta returned nil, want %q", tc.wantErr)
			case tc.wantErr != "" && err != nil && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("CheckTypeMeta returned %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestDurationRoundTrip(t *testing.T) {
	t.Parallel()

	var d v1alpha1.Duration
	if err := json.Unmarshal([]byte(`"90s"`), &d); err != nil {
		t.Fatalf("unmarshalling a duration: %v", err)
	}
	if got, want := d.Get(), 90*time.Second; got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshalling a duration: %v", err)
	}
	if got, want := string(out), `"1m30s"`; got != want {
		t.Errorf("marshalled = %s, want %s", got, want)
	}
}

func TestDurationRejectsBadValues(t *testing.T) {
	t.Parallel()

	// A bare number reads as nanoseconds, which is never what a
	// configuration means, so it is rejected instead.
	for _, raw := range []string{`30`, `"forever"`, `"-5s"`, `true`} {
		var d v1alpha1.Duration
		if err := json.Unmarshal([]byte(raw), &d); err == nil {
			t.Errorf("unmarshalling %s should fail", raw)
		}
	}
}

// A duration reads from YAML and writes to it as it does with JSON: as a
// string such as "90s", never as a bare number, which reads as nanoseconds,
// and never negative.
func TestDurationReadsAndWritesYAML(t *testing.T) {
	t.Parallel()

	type config struct {
		Timeout v1alpha1.Duration `yaml:"timeout"`
	}
	var got config
	if err := yaml.Unmarshal([]byte("timeout: 90s\n"), &got); err != nil {
		t.Fatalf("unmarshalling a duration: %v", err)
	}
	if want := 90 * time.Second; got.Timeout.Get() != want {
		t.Errorf("duration = %v, want %v", got.Timeout.Get(), want)
	}
	out, err := yaml.Marshal(got)
	if err != nil {
		t.Fatalf("marshalling a duration: %v", err)
	}
	if want := "timeout: 1m30s\n"; string(out) != want {
		t.Errorf("marshalled = %q, want %q", out, want)
	}

	// A scalar is read as text and refused by what it says; a list or a
	// mapping is refused by the decoder, at its line and column.
	for raw, scalar := range map[string]bool{
		"30": true, "forever": true, "-5s": true, "true": true, "[1s]": false, "{seconds: 30}": false,
	} {
		var c config
		err := yaml.Unmarshal([]byte("timeout: "+raw+"\n"), &c)
		switch {
		case err == nil:
			t.Errorf("unmarshalling %s gave %v, want an error", raw, c.Timeout)
		case scalar && !strings.Contains(err.Error(), "invalid duration"):
			t.Errorf("unmarshalling %s: %v; want it to say what a duration is", raw, err)
		}
	}
}

func TestDurationOr(t *testing.T) {
	t.Parallel()

	var zero v1alpha1.Duration
	if got, want := zero.Or(time.Minute), time.Minute; got != want {
		t.Errorf("Or on a zero duration = %v, want %v", got, want)
	}
	set := v1alpha1.Duration(2 * time.Second)
	if got, want := set.Or(time.Minute), 2*time.Second; got != want {
		t.Errorf("Or on a set duration = %v, want %v", got, want)
	}
}

func TestKindsAreComplete(t *testing.T) {
	t.Parallel()

	if got, want := len(v1alpha1.Kinds()), 6; got != want {
		t.Errorf("Kinds() has %d entries, want %d", got, want)
	}
	if !v1alpha1.KnownKind(v1alpha1.KindNodeInventory) || v1alpha1.KnownKind("Pod") {
		t.Error("KnownKind does not agree with Kinds()")
	}
}
