// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// cincOverride points the configuration file of the nodes at path, so that
// a test can run the script cinc config sends against a real shell.
func cincOverride(t *testing.T, path string) string {
	t.Helper()
	dir := t.TempDir()
	override := filepath.Join(dir, "override.yaml")
	if err := os.WriteFile(override, []byte(`apiVersion: clusterctl/v1alpha1
kind: Config
contexts:
  - name: cluster1
    cluster: cluster1
    overrides:
      services.cinc.soloConfigPath: `+path+`
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// shellReply runs every script it is sent in a real shell, in dir, with the
// payload the command streamed to it, which is what a node would do. keep
// decides how much of the payload arrives, so a dropped connection can be
// played.
func shellReply(t *testing.T, dir string, keep func([]byte) []byte) func(transport.Target, transport.Request) (*transport.Result, error) {
	return func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		var payload []byte
		if req.Stdin != nil {
			var err error
			if payload, err = io.ReadAll(req.Stdin); err != nil {
				return nil, err
			}
		}
		if keep != nil {
			payload = keep(payload)
		}
		cmd := exec.Command("bash", "-c", req.Script)
		cmd.Dir = dir
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		res := &transport.Result{Target: tg, Stdout: stdout.String(), Stderr: stderr.String()}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, fmt.Errorf("%s: command exited %d", tg, res.ExitCode)
		}
		return res, err
	}
}

// sourced reads the file back the way the shell toolkit and anything else
// on the node reads it: by sourcing it under set -eu.
func sourced(t *testing.T, dir, path string) (url, runList string) {
	t.Helper()
	cmd := exec.Command("sh", "-c",
		`set -eu; . "$1"; printf '%s\n%s' "$CHEF_RECIPE_URL" "${CHEF_RUN_LIST-}"`, "sh", path)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sourcing %s failed: %v\n%s", path, err, out)
	}
	url, runList, _ = strings.Cut(string(out), "\n")
	return url, runList
}

// Report 1.5: the values went into the file unquoted, and the file is
// sourced as root. A query string sent the assignment to the background, a
// run list with a space ran its second half as a command, and a command
// substitution in the URL ran on the node.
func TestCincConfigWritesAFileSafeToSource(t *testing.T) {
	tests := []struct {
		name, url, runList string
	}{
		{"a query string", "http://installer/cinc/a.tgz?v=1&t=2", ""},
		{"a run list with a space", "http://installer/cinc/latest.tgz", "role[base], role[exe]"},
		{"a command substitution", "http://installer/cinc/$(touch${IFS}PWNED).tgz", "role[exe];touch PWNED"},
		{"backticks and an apostrophe", "https://installer/cinc/`touch${IFS}PWNED`/it's.tgz", "recipe[a'b]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "etc", "cinc", "solo")
			args := []string{"cinc", "config", tc.url, "-n", "exe0001", "--yes"}
			if tc.runList != "" {
				args = append(args, "--run-list", tc.runList)
			}
			rec := &transport.Recorder{Reply: shellReply(t, dir, nil)}
			h, err := run(t, harnessOptions{recorder: rec, config: []string{cincOverride(t, path)}}, args...)
			if err != nil {
				t.Fatalf("cinc config failed: %v\n%s%s", err, h.out, h.errOut)
			}
			url, runList := sourced(t, dir, path)
			if url != tc.url || runList != tc.runList {
				t.Errorf("sourced back %q and %q, want %q and %q", url, runList, tc.url, tc.runList)
			}
			if _, err := os.Stat(filepath.Join(dir, "PWNED")); err == nil {
				t.Error("a value was run as a command")
			}

			// And cinc run, which does not source it, reads the same.
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			solo, err := parseCincSolo(string(content))
			if err != nil {
				t.Fatalf("parsing what cinc config wrote: %v\n%s", err, content)
			}
			if solo.URL != tc.url || solo.RunList != tc.runList {
				t.Errorf("parsed %+v, want %q and %q", solo, tc.url, tc.runList)
			}
		})
	}
}

func TestCincConfigRejectsWhatIsNotAnArchiveURL(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no scheme", []string{"installer/cinc/latest.tgz"}},
		{"another scheme", []string{"file:///srv/cinc/latest.tgz"}},
		{"no host", []string{"http:///cinc/latest.tgz"}},
		{"a space", []string{"http://installer/cinc/$(touch PWNED).tgz"}},
		{"a newline", []string{"http://installer/cinc/latest.tgz\nreboot"}},
		{"a newline in the run list", []string{"http://installer/cinc/latest.tgz", "--run-list", "role[exe]\nreboot"}},
		{"a NUL in the run list", []string{"http://installer/cinc/latest.tgz", "--run-list", "role[exe]\x00"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"cinc", "config"}, tc.args...)
			args = append(args, "-n", "exe0001", "--yes")
			h, err := run(t, harnessOptions{}, args...)
			if got := exitcode.From(err); got != exitcode.Usage {
				t.Errorf("exit code = %d (%v), want %d", got, err, exitcode.Usage)
			}
			if calls := h.recorder.Commands(); len(calls) != 0 {
				t.Errorf("something was sent: %q", calls)
			}
		})
	}
}

// Report 8.8: the file was emptied with install and then filled by cat, so a
// connection that dropped in between left an empty file, and install -D
// created missing directories whatever the umask said.
func TestCincConfigReplacesTheFileWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "cinc", "solo")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const old = "CHEF_RECIPE_URL=http://installer/cinc/old.tgz\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	// Half the payload arrives, then standard input closes.
	half := func(p []byte) []byte { return p[:len(p)/2] }
	rec := &transport.Recorder{Reply: shellReply(t, dir, half)}
	h, err := run(t, harnessOptions{recorder: rec, config: []string{cincOverride(t, path)}},
		"cinc", "config", "http://installer/cinc/new.tgz", "-n", "exe0001", "--yes")
	if err == nil {
		t.Fatalf("a truncated write was reported as a success:\n%s", h.out)
	}
	if got, _ := os.ReadFile(path); string(got) != old {
		t.Errorf("the file holds %q after a failed write, want the old %q", got, old)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("the failed write left files behind: %v", entries)
	}

	// Into a directory that does not exist yet, with a strict umask on the
	// node: the directories are made for a world readable file.
	fresh := filepath.Join(dir, "new", "cinc", "solo")
	rec = &transport.Recorder{Reply: shellReply(t, dir, nil)}
	if _, err := run(t, harnessOptions{recorder: rec, config: []string{cincOverride(t, fresh)}},
		"cinc", "config", "http://installer/cinc/new.tgz", "-n", "exe0001", "--yes"); err != nil {
		t.Fatalf("cinc config failed: %v", err)
	}
	for p, want := range map[string]os.FileMode{
		fresh:                             0o644,
		filepath.Dir(fresh):               0o755 | os.ModeDir,
		filepath.Dir(filepath.Dir(fresh)): 0o755 | os.ModeDir,
	} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s has mode %v, want %v", p, info.Mode(), want)
		}
	}
}

// cincNode answers the read of the configuration file with content, and
// records the client run it is sent.
func cincNode(content string) *transport.Recorder {
	return &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if req.Script != "" && strings.Contains(req.Script, "cat") {
			return &transport.Result{Target: tg, Stdout: content}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
}

// clientRuns returns the argument vectors of the client runs a recorder saw.
func clientRuns(rec *transport.Recorder) [][]string {
	var out [][]string
	for _, c := range rec.Calls() {
		if len(c.Request.Argv) > 0 {
			out = append(out, c.Request.Argv)
		}
		if strings.Contains(c.Request.Script, "cinc-solo") {
			out = append(out, []string{"sh", "-c", c.Request.Script})
		}
	}
	return out
}

// Reports 1.5 and 5.11: cinc run sourced the file as root under set -eu, so
// a value in it ran as a command, and a file without a run list, which is
// what cinc config writes without --run-list, stopped every run with
// "CHEF_RUN_LIST: unbound variable".
func TestCincRunPassesTheFileAsArguments(t *testing.T) {
	tests := []struct {
		name, content string
		flags         []string
		want          []string
	}{
		{
			name:    "without a run list",
			content: "CHEF_RECIPE_URL=http://installer/cinc/latest.tgz\n",
			want:    []string{"cinc-solo", "--minimal-ohai", "--recipe-url", "http://installer/cinc/latest.tgz"},
		},
		{
			name:    "with the run list of the file",
			content: "CHEF_RECIPE_URL='http://installer/cinc/a.tgz?v=1&t=2'\nCHEF_RUN_LIST='role[base], role[exe]'\n",
			want: []string{"cinc-solo", "--minimal-ohai", "--recipe-url", "http://installer/cinc/a.tgz?v=1&t=2",
				"--override-runlist", "role[base], role[exe]"},
		},
		{
			name:    "with a run list for this run",
			content: "CHEF_RECIPE_URL=http://installer/cinc/latest.tgz\nCHEF_RUN_LIST=role[exe]\n",
			flags:   []string{"--run-list", "recipe[x]"},
			want: []string{"cinc-solo", "--minimal-ohai", "--recipe-url", "http://installer/cinc/latest.tgz",
				"--override-runlist", "recipe[x]"},
		},
		{
			name:    "with a command substitution, quoted",
			content: "# written by hand\nexport CHEF_RECIPE_URL='http://installer/cinc/$(touch${IFS}PWNED).tgz'\n",
			want:    []string{"cinc-solo", "--minimal-ohai", "--recipe-url", "http://installer/cinc/$(touch${IFS}PWNED).tgz"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := cincNode(tc.content)
			args := append([]string{"cinc", "run", "-n", "exe0001", "--yes"}, tc.flags...)
			h, err := run(t, harnessOptions{recorder: rec}, args...)
			if err != nil {
				t.Fatalf("cinc run failed: %v\n%s%s", err, h.out, h.errOut)
			}
			runs := clientRuns(rec)
			if len(runs) != 1 || !reflect.DeepEqual(runs[0], tc.want) {
				t.Errorf("client runs = %q, want %q", runs, tc.want)
			}
		})
	}
}

// A file that would do more than assign when sourced, or names no archive,
// is refused on that node, and the client is not started there.
func TestCincRunRefusesAFileThatIsNotPlainAssignments(t *testing.T) {
	for name, content := range map[string]string{
		"an unquoted command substitution": "CHEF_RECIPE_URL=http://installer/cinc/$(touch PWNED).tgz\n",
		"backticks in double quotes":       "CHEF_RECIPE_URL=\"http://installer/`id`.tgz\"\n",
		"a second command":                 "CHEF_RECIPE_URL=http://installer/cinc/latest.tgz; reboot\n",
		"a run list split by a space":      "CHEF_RECIPE_URL=http://installer/cinc/latest.tgz\nCHEF_RUN_LIST=role[base], role[exe]\n",
		"no archive":                       "CHEF_RUN_LIST=role[exe]\n",
		"an archive that is not a URL":     "CHEF_RECIPE_URL='file:///srv/latest.tgz'\n",
		"no file":                          "",
	} {
		t.Run(name, func(t *testing.T) {
			rec := cincNode(content)
			h, err := run(t, harnessOptions{recorder: rec}, "cinc", "run", "-n", "exe0001", "--yes")
			if got := exitcode.From(err); got != exitcode.TargetFailed {
				t.Errorf("exit code = %d (%v), want %d\n%s", got, err, exitcode.TargetFailed, h.out)
			}
			if runs := clientRuns(rec); len(runs) != 0 {
				t.Errorf("the client was started: %q", runs)
			}
		})
	}
}

// Report 10.7: cinc show listed a node it could not reach as "not
// configured" and exited 0.
func TestCincShowReportsNodesItCouldNotRead(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		switch tg.Name {
		case "exe0003":
			const detail = "ssh: connect to host exe0003 port 22: No route to host"
			return &transport.Result{Target: tg, ExitCode: 255, Stderr: detail + "\n"},
				exitcode.Wrap(exitcode.Transport, fmt.Errorf("%s: %s", tg, detail))
		case "exe0004":
			// The file does not exist: the node is reachable and simply
			// not configured.
			return &transport.Result{Target: tg}, nil
		}
		return &transport.Result{Target: tg,
			Stdout: "CHEF_RECIPE_URL='http://installer/cinc/a.tgz?v=1&t=2'\nCHEF_RUN_LIST=role[exe]\n"}, nil
	}}

	h, err := run(t, harnessOptions{recorder: rec}, "cinc", "show", "-n", "exe[1-4]")
	if got := exitcode.From(err); got != exitcode.Transport {
		t.Errorf("exit code = %d (%v), want %d", got, err, exitcode.Transport)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(h.out.String(), "\n") {
		if name, rest, ok := strings.Cut(line, " "); ok {
			rows[name] = rest
		}
	}
	if !strings.Contains(rows["exe0003"], "No route to host") || strings.Contains(rows["exe0003"], "not configured") {
		t.Errorf("exe0003 row = %q, want the transport failure", rows["exe0003"])
	}
	if !strings.Contains(rows["exe0004"], "not configured") {
		t.Errorf("exe0004 row = %q, want it not configured", rows["exe0004"])
	}
	if !strings.Contains(rows["exe0001"], "http://installer/cinc/a.tgz?v=1&t=2") {
		t.Errorf("exe0001 row = %q, want the unquoted URL", rows["exe0001"])
	}

	h, _ = run(t, harnessOptions{recorder: rec}, "-o", "json", "cinc", "show", "-n", "exe[1-4]")
	var objects []map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &objects); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.out)
	}
	byNode := map[string]map[string]any{}
	for _, o := range objects {
		byNode[fmt.Sprint(o["node"])] = o
	}
	if len(byNode) != 4 {
		t.Fatalf("JSON holds %d nodes, want all 4:\n%s", len(byNode), h.out)
	}
	if e, _ := byNode["exe0003"]["error"].(string); !strings.Contains(e, "No route to host") {
		t.Errorf("exe0003 = %v, want an error field", byNode["exe0003"])
	}
	if c, _ := byNode["exe0004"]["configured"].(bool); c || byNode["exe0004"]["error"] != nil {
		t.Errorf("exe0004 = %v, want it reported as not configured without an error", byNode["exe0004"])
	}
	if byNode["exe0001"]["archive"] != "http://installer/cinc/a.tgz?v=1&t=2" || byNode["exe0001"]["runList"] != "role[exe]" {
		t.Errorf("exe0001 = %v", byNode["exe0001"])
	}
}

// A file that is not plain assignments is shown as such rather than as what
// sourcing it would have produced, and counts as a failure.
func TestCincShowFlagsAFileItCannotRead(t *testing.T) {
	rec := cincNode("CHEF_RECIPE_URL=http://installer/$(id)\x1b[2J.tgz\n")
	h, err := run(t, harnessOptions{recorder: rec}, "cinc", "show", "-n", "exe0001")
	if got := exitcode.From(err); got != exitcode.TargetFailed {
		t.Errorf("exit code = %d (%v), want %d", got, err, exitcode.TargetFailed)
	}
	if strings.Contains(h.out.String(), "\x1b") {
		t.Errorf("a control character from the node reached the terminal: %q", h.out)
	}
}

// Whatever cinc config quotes, cinc run reads back unchanged, and the plain
// values the shell toolkit wrote are read as the shell read them.
func TestCincFileRoundTrips(t *testing.T) {
	for _, value := range []string{
		"role[exe]", "role[base], role[exe]", "it's", `a"b`, `back\slash`, "$(id)", "`id`",
		"a;b&c|d<e>f(g)~h", "#hash", "",
	} {
		got, err := shellLiteral(shellquote.Quote(value))
		if err != nil || got != value {
			t.Errorf("shellLiteral(Quote(%q)) = %q, %v", value, got, err)
		}
	}
	for in, want := range map[string]string{
		`role[exe]`:                "role[exe]",
		`"role[base], role[exe]"`:  "role[base], role[exe]",
		`a\ b`:                     "a b",
		`http://x/a.tgz # comment`: "http://x/a.tgz",
		`'it'\''s'`:                "it's",
		`http://x/a#frag`:          "http://x/a#frag",
		`http://x/{a,b}*.tgz?[1]!`: "http://x/{a,b}*.tgz?[1]!",
	} {
		if got, err := shellLiteral(in); err != nil || got != want {
			t.Errorf("shellLiteral(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
}

// A dry run stops at the preview, before each node's file is read, so the
// preview says what it could not look at.
func TestCincRunDryRunNamesTheReadItSkips(t *testing.T) {
	h, err := run(t, harnessOptions{}, "cinc", "run", "-n", "exe0001", "--dry-run")
	if err != nil {
		t.Fatalf("cinc run --dry-run failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "from each node's /etc/cinc/solo, which a dry run does not read") {
		t.Errorf("the preview does not name the skipped read:\n%s", h.errOut)
	}
	if calls := h.recorder.Commands(); len(calls) != 0 {
		t.Errorf("a dry run sent %q", calls)
	}
}
