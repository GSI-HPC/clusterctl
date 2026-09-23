// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package shellquote_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/shellquote"
)

func TestQuote(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"plain":       "plain",
		"exe0001":     "exe0001",
		"/usr/bin/ls": "/usr/bin/ls",
		"":            "''",
		"two words":   "'two words'",
		"*.log":       "'*.log'",
		"it's":        `'it'\''s'`,
		"a\nb":        "'a\nb'",
		"$HOME":       "'$HOME'",
		"a;rm -rf /":  "'a;rm -rf /'",
	}
	for in, want := range tests {
		if got := shellquote.Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestQuoteSurvivesTheShell is the property that matters: whatever is quoted
// here comes back out of a real shell unchanged.
func TestQuoteSurvivesTheShell(t *testing.T) {
	t.Parallel()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell available to check against")
	}

	vectors := [][]string{
		{"echo", "hello"},
		{"echo", "*.log"},
		{"echo", "it's a trap"},
		{"echo", "a  b"},
		{"echo", "$HOME", "`id`", "$(id)"},
		{"echo", "exe[01-10]"},
		{"echo", "a\\b"},
		{"echo", "-n", ""},
		{"echo", "tab\there"},
	}
	for _, argv := range vectors {
		line := shellquote.Join(argv)
		// printf prints each argument on its own line, so the shell's view
		// of the argument vector can be compared with the original.
		script := "printf '%s\\n' " + strings.TrimPrefix(line, "echo ")
		out, err := exec.Command(sh, "-c", script).Output()
		if err != nil {
			t.Fatalf("running %q: %v", script, err)
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		want := argv[1:]
		if len(got) != len(want) {
			t.Errorf("%q produced %d arguments, want %d: %q", line, len(got), len(want), got)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: argument %d = %q, want %q", line, i, got[i], want[i])
			}
		}
	}
}

func TestSplit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want []string
	}{
		{"sinfo -h -o %N", []string{"sinfo", "-h", "-o", "%N"}},
		{`sinfo -o "%N %T"`, []string{"sinfo", "-o", "%N %T"}},
		{`echo 'it'\''s'`, []string{"echo", "it's"}},
		{`a\ b c`, []string{"a b", "c"}},
		{"   ", nil},
	}
	for _, tc := range tests {
		got, err := shellquote.Split(tc.in)
		if err != nil {
			t.Errorf("Split(%q) failed: %v", tc.in, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("Split(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, in := range []string{`"unterminated`, `'unterminated`} {
		if _, err := shellquote.Split(in); err == nil {
			t.Errorf("Split(%q) should fail", in)
		}
	}
}

func TestJoinRoundTrips(t *testing.T) {
	t.Parallel()

	argv := []string{"bash", "-c", "echo 'a b' && ls *.log", "arg with spaces"}
	got, err := shellquote.Split(shellquote.Join(argv))
	if err != nil {
		t.Fatalf("Split failed: %v", err)
	}
	if strings.Join(got, "\x00") != strings.Join(argv, "\x00") {
		t.Errorf("round trip gave %q, want %q", got, argv)
	}
}
