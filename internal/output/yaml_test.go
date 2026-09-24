// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output_test

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/GSI-HPC/clusterctl/internal/output"
)

// TestYAMLKeepsIntegers covers review finding 12.3: exitCode printed as 0.0
// and a PID of 1234567 as 1.234567e+06.
func TestYAMLKeepsIntegers(t *testing.T) {
	t.Parallel()

	r := output.Result{Object: map[string]any{
		"exitCode": 0, "pid": 1234567, "big": uint64(9007199254740993),
		"ratio": 0.5, "huge": 1e21, "neg": -3,
	}}
	got := render(t, "yaml", r)
	for _, want := range []string{
		"exitCode: 0\n", "pid: 1234567\n", "big: 9007199254740993\n",
		"ratio: 0.5\n", "huge: 1.0e+21\n", "neg: -3\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("-o yaml is missing %q:\n%s", want, got)
		}
	}
}

// TestYAMLQuotesWhatAResolverWouldMisread covers review finding 12.3: .inf,
// .nan and "? x" were left plain, ESC was written raw and a CR was lost in a
// block literal. Every string has to read back as the same string.
func TestYAMLQuotesWhatAResolverWouldMisread(t *testing.T) {
	t.Parallel()

	values := []string{
		"", " ", "plain", "exe0001", "yes", "No", "on", "OFF", "y", "n", "true",
		"null", "Null", "~", ".inf", "-.inf", "+.INF", ".nan", ".NaN", "? x",
		"- x", "-x", ": x", "x: y", "x:", "x #y", "#x", "&a", "*a", "!tag",
		"|", ">", "%x", "@x", "`x", "'x'", `"x"`, "[x]", "{x}", "x,y",
		"0", "0.0", "1e3", "0x1f", "0o17", "0b101", "1_000", "12:30", "1.2.3",
		"2026-09-24", "2026-09-24T10:00:00Z", "10.0.0.1", "=", "<<",
		"esc\x1b[31m", "bell\x07", "nul\x00", "del\x7f", "c1\u0085x", "ls\u2028x",
		"bidi\u202ex", "bom\ufeffx", "tab\tx", "trailing ", " leading",
		"cr\r\n", "one\ntwo\n", "one\ntwo", "one\ntwo\n\n", "\nleading newline",
		"  indented\nsecond\n", "\n", "\n\n", "last line \n", "a\r\nb\r\n",
		"--- x", "...", "---", "line\n---\n...\n", "x\n  y\n", "grüße",
		"http://example.org/a#b", "a: b\nc: d\n", "ok\r\x1b[1Aevil",
	}
	for _, v := range values {
		in := map[string]any{"v": v, "list": []any{v, map[string]any{"k": v}}, v: 1}
		got := render(t, "yaml", output.Result{Object: in})
		if strings.ContainsAny(got, "\x1b\x07\x00\x7f\r\u0085\u2028\u202e\ufeff") {
			t.Errorf("-o yaml wrote a control character for %q:\n%q", v, got)
		}
		var back map[string]any
		if err := yaml.Unmarshal([]byte(got), &back); err != nil {
			t.Errorf("-o yaml for %q does not parse: %v\n%s", v, err, got)
			continue
		}
		if s, ok := back["v"].(string); !ok || s != v {
			t.Errorf("-o yaml for %q reads back as %#v:\n%s", v, back["v"], got)
		}
		list, _ := back["list"].([]any)
		if len(list) != 2 || list[0] != v {
			t.Errorf("-o yaml for %q reads back the list as %#v:\n%s", v, back["list"], got)
		} else if m, _ := list[1].(map[string]any); m["k"] != v {
			t.Errorf("-o yaml for %q reads back the nested map as %#v:\n%s", v, list[1], got)
		}
		if _, ok := back[v]; !ok {
			t.Errorf("-o yaml for %q lost the key:\n%s", v, got)
		}
	}
}

// TestYAMLLayout pins the shape of the output, which is what the goccy
// encoder printed before.
func TestYAMLLayout(t *testing.T) {
	t.Parallel()

	r := output.Result{Object: []any{
		map[string]any{
			"target":   map[string]any{"name": "exe0001", "host": "exe0001.example.org"},
			"exitCode": 0,
			"stdout":   "line one\nline two\n",
			"list":     []any{"a", []any{"b", "c"}, map[string]any{}, []any{}},
		},
	}}
	want := `- exitCode: 0
  list:
  - a
  - - b
    - c
  - {}
  - []
  stdout: |
    line one
    line two
  target:
    host: exe0001.example.org
    name: exe0001
`
	if got := render(t, "yaml", r); got != want {
		t.Errorf("-o yaml =\n%s\nwant\n%s", got, want)
	}
	if got, want := render(t, "yaml", output.Result{Object: []any{}}), "[]\n"; got != want {
		t.Errorf("-o yaml of an empty list = %q, want %q", got, want)
	}
	if got, want := render(t, "yaml", output.Result{Object: "x\ny"}), "|-\n  x\n  y\n"; got != want {
		t.Errorf("-o yaml of a string = %q, want %q", got, want)
	}
}
