// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
)

// The configuration was read with goccy/go-yaml, and is read with yaml.v3,
// which takes more for numbers and times than goccy did. A scalar reads as
// it did: a number only when every YAML reader takes it for one, and text
// otherwise, which a field that takes a number refuses.
func TestScalarsReadAsTheyDid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		want  any
	}{
		{"", nil}, {"~", nil}, {"null", nil}, {"NULL", nil},
		{"true", true}, {"False", false}, {"yes", "yes"}, {"off", "off"},
		{"0", int64(0)}, {"42", int64(42)}, {"-1", int64(-1)}, {"+1", int64(1)},
		{"0x1F", int64(31)}, {"0xBEEF", int64(48879)}, {"-0x1F", int64(-31)}, {"0o17", int64(15)}, {"0b101", int64(5)},
		{"1_000", int64(1000)}, {"0B101", "0B101"},
		{"9223372036854775807", int64(9223372036854775807)}, {"18446744073709551615", "18446744073709551615"},
		{"0600", "0600"}, {"-0600", "-0600"}, {"08", "08"}, {"00", "00"},
		{"1.5", 1.5}, {".5", 0.5}, {"1.5e-3", 0.0015}, {"1.", 1.0},
		{"1e3", "1e3"}, {"1E3", "1E3"}, {"0X1F", "0X1F"}, {"0O17", "0O17"},
		{"18446744073709551616", "18446744073709551616"}, {"-9223372036854775809", "-9223372036854775809"},
		{"2026-09-01", "2026-09-01"}, {"2026-09-01T10:00:00Z", "2026-09-01T10:00:00Z"}, {"10:30", "10:30"},
		{"30s", "30s"}, {"'0600'", "0600"}, {`"true"`, "true"}, {"'null'", "null"},
		{"!!str 123", int64(123)}, {"!!int '8'", "8"}, {"!foo 12", "12"}, {"!foo", nil}, {"!!binary aGVsbG8=", "aGVsbG8="},
		{"|\n  text\n", "text\n"}, {">-\n  folded\n  text\n", "folded text"}, {"<<", "<<"},
	} {
		docs, err := config.ParseDocuments("t.yaml", []byte("v: "+tc.value+"\n"))
		if err != nil {
			t.Errorf("v: %s: %v", tc.value, err)
			continue
		}
		if got := docs[0].Data["v"]; fmt.Sprintf("%T %v", got, got) != fmt.Sprintf("%T %v", tc.want, tc.want) {
			t.Errorf("v: %s reads as %T %v, want %T %v", tc.value, got, got, tc.want, tc.want)
		}
	}
}

// A key is text, written without a tag: a key 8 or true would be a number
// or a boolean to another reader. Text that yaml.v3 takes for a number or a
// time, and goccy did not, is a key as it was.
func TestAMappingKeyIsText(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"name: 1", "'8': 1", `"true": 1`, "08: 1", "1e3: 1", "0X1F: 1", "2026-09-01: 1", "yes: 1", "? name\n: 1"} {
		if _, err := config.ParseDocuments("t.yaml", []byte(src+"\n")); err != nil {
			t.Errorf("%q was refused: %v", src, err)
		}
	}
	for _, key := range []string{"8", "0600", "true", "null", "~", "1.5", "<<", "!!str x", "!foo x", "[a]", "*a"} {
		_, err := config.ParseDocuments("t.yaml", []byte("a: &a x\n"+key+": 1\n"))
		if err == nil || !strings.Contains(err.Error(), "must be a string") && !strings.Contains(err.Error(), "aliases") {
			t.Errorf("the key %s was taken: %v", key, err)
		}
	}
}

// What the configuration refuses, some of which goccy took although the YAML
// specification does not allow it.
func TestParseDocumentsRefusesWhatYAMLDoesNotAllow(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		"v: .inf\n", "v: -.inf\n", "v: +.inf\n", "v: .nan\n",
		"v: [|\n  text\n]\n", "v: [- a]\n", "a:\n  b: %x\n", "v: %x\n", "...\n",
	} {
		if _, err := config.ParseDocuments("t.yaml", []byte(src)); err == nil {
			t.Errorf("%q was read", src)
		}
	}
}

// A byte order mark, which some editors write, was read as part of the
// first key, and a column after a tag was counted one short.
func TestParseDocumentsReadsAByteOrderMarkAndATagAsWritten(t *testing.T) {
	t.Parallel()
	docs, err := config.ParseDocuments("t.yaml", []byte("\ufeffapiVersion: clusterctl/v1alpha1\nkind: Site\nspec: !!map {fanout: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if docs[0].Kind != "Site" || docs[0].APIVersion != "clusterctl/v1alpha1" {
		t.Errorf("the document reads as %q %q", docs[0].APIVersion, docs[0].Kind)
	}
	if got := docs[0].Position("spec.fanout"); got.Line != 3 || got.Column != 14 {
		t.Errorf("spec.fanout is at %d:%d, want 3:14", got.Line, got.Column)
	}
}
