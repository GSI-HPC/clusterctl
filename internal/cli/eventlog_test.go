// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/internal/version"
)

// traceparent is the trace context a CI job hands clusterctl in the tests.
const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// spanIDs matches the id of a span in an event log.
var spanIDs = regexp.MustCompile(`"span":"([0-9a-f]{16})"`)

// runs matches the run of a line of an event log, and versions the version
// that wrote it.
var (
	runs     = regexp.MustCompile(`"run":"[0-9a-f]{16}"`)
	versions = regexp.MustCompile(`"version":"[^"]*"`)
)

// normalised returns an event log with the id of each span replaced by
// "#n", n in the order the spans first appear, wherever the id is written,
// the run by "#run" and the version by "VERSION".
func normalised(log string) string {
	log = runs.ReplaceAllString(log, `"run":"#run"`)
	log = versions.ReplaceAllString(log, `"version":"VERSION"`)
	var ids []string
	seen := map[string]bool{}
	for _, m := range spanIDs.FindAllStringSubmatch(log, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, `"`+m[1]+`"`, fmt.Sprintf(`"#%d"`, len(seen)))
		}
	}
	return strings.NewReplacer(ids...).Replace(log)
}

// readLog returns what an event log holds.
func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	return string(data)
}

// exe0002Fails answers exec's command on every node but exe0002, which says
// no.
func exe0002Fails(tg transport.Target, _ transport.Request) (*transport.Result, error) {
	if tg.Name == "exe0002" {
		return transport.ExitResult(tg, 1, "", "uptime: not found\n"), nil
	}
	return &transport.Result{Target: tg, Stdout: "up 3 days\n"}, nil
}

// The trace context clusterctl is started with is the event log's to
// continue, and no program clusterctl runs is handed it: they inherit an
// environment without TRACEPARENT and TRACESTATE, as ssh, scp, sops and a
// credential helper do.
func TestTheProgramsClusterctlRunsAreHandedNoTraceContext(t *testing.T) {
	t.Setenv("TRACEPARENT", traceparent)
	t.Setenv("TRACESTATE", "ci=build-42")
	path := filepath.Join(t.TempDir(), "progress.jsonl")
	if _, err := run(t, harnessOptions{unwatched: true}, "--progress-log", path, "exec", "-n", "exe[1-2]", "-y", "--", "uptime"); err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(readLog(t, path), "\n"); !strings.Contains(first, `"parent":"00f067aa0ba902b7"`) {
		t.Errorf("the log begins %s, want the trace TRACEPARENT names continued", first)
	}
	out, err := exec.Command("sh", "-c", `printf '%s %s' "${TRACEPARENT-unset}" "${TRACESTATE-unset}"`).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "unset unset" {
		t.Errorf("a program clusterctl runs sees TRACEPARENT and TRACESTATE as %q, want neither", out)
	}
}

// --progress-log appends the events of a command to a file only its owner
// can read, one line each, after the trace they belong to, which is the
// one TRACEPARENT names when a CI job hands one on. Nothing else changes:
// with no display, standard output and standard error hold what they hold
// without the log.
func TestTheEventLogOfACommand(t *testing.T) {
	t.Setenv("TRACEPARENT", traceparent)
	t.Setenv("TRACESTATE", "ci=build-42")
	c := fakeDisplays(t)
	reply := func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		c.draw()
		return exe0002Fails(tg, req)
	}
	args := []string{"--fanout", "1", "exec", "-n", "exe[1-3]", "-y", "--", "uptime"}
	path := filepath.Join(t.TempDir(), "progress.jsonl")
	h, err := run(t, harnessOptions{unwatched: true, recorder: &transport.Recorder{Reply: reply}},
		append([]string{"--progress-log", path}, args...)...)
	wantCode(t, err, exitcode.TargetFailed)
	quiet, quietErr := run(t, harnessOptions{unwatched: true, recorder: &transport.Recorder{Reply: reply}}, args...)
	if h.out.String() != quiet.out.String() || h.errOut.String() != quiet.errOut.String() || fmt.Sprint(err) != fmt.Sprint(quietErr) {
		t.Errorf("with an event log, exec printed\n%q\n%q\nand returned %v, want\n%q\n%q\n%v",
			h.out, h.errOut, err, quiet.out, quiet.errOut, quietErr)
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the event log was created with mode %v, want 0600", info.Mode().Perm())
	}
	got := normalised(readLog(t, path))
	want := `{"v":1,"type":"trace","run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","parent":"00f067aa0ba902b7","traceFlags":"01","traceState":"ci=build-42","version":"VERSION"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":1,"time":"2026-09-26T12:00:00.000000000Z","type":"start","span":"#1","kind":"command","name":"exec","state":"running"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":2,"time":"2026-09-26T12:00:00.000000000Z","type":"start","span":"#2","parent":"#1","kind":"step","name":"run","flags":["fold","show-lines"],"state":"running","total":3,"limit":1}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":3,"time":"2026-09-26T12:00:00.000000000Z","type":"start","span":"#3","parent":"#2","kind":"target","name":"exe0001","flags":["show-lines"],"state":"queued","node":"exe0001","host":"exe0001.hpc.example.org"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":4,"time":"2026-09-26T12:00:00.000000000Z","type":"start","span":"#4","parent":"#2","kind":"target","name":"exe0002","flags":["show-lines"],"state":"queued","node":"exe0002","host":"exe0002.hpc.example.org"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":5,"time":"2026-09-26T12:00:00.000000000Z","type":"start","span":"#5","parent":"#2","kind":"target","name":"exe0003","flags":["show-lines"],"state":"queued","node":"exe0003","host":"exe0003.hpc.example.org"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":6,"time":"2026-09-26T12:00:00.000000000Z","type":"run","span":"#3","parent":"#2","kind":"target","name":"exe0001","flags":["show-lines"],"state":"running","node":"exe0001","host":"exe0001.hpc.example.org"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":7,"time":"2026-09-26T12:00:00.000000000Z","type":"start","span":"#6","parent":"#3","kind":"call","name":"ssh","flags":["show-lines"],"state":"running","node":"exe0001","host":"exe0001.hpc.example.org","timeout":600}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":8,"time":"2026-09-26T12:00:01.000000000Z","type":"end","span":"#6","parent":"#3","kind":"call","name":"ssh","flags":["show-lines"],"state":"ended","node":"exe0001","host":"exe0001.hpc.example.org","timeout":600,"exit":0,"status":"ok"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":9,"time":"2026-09-26T12:00:01.000000000Z","type":"end","span":"#3","parent":"#2","kind":"target","name":"exe0001","flags":["show-lines"],"state":"ended","node":"exe0001","host":"exe0001.hpc.example.org","status":"ok"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":10,"time":"2026-09-26T12:00:01.000000000Z","type":"run","span":"#4","parent":"#2","kind":"target","name":"exe0002","flags":["show-lines"],"state":"running","node":"exe0002","host":"exe0002.hpc.example.org"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":11,"time":"2026-09-26T12:00:01.000000000Z","type":"start","span":"#7","parent":"#4","kind":"call","name":"ssh","flags":["show-lines"],"state":"running","node":"exe0002","host":"exe0002.hpc.example.org","timeout":600}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":12,"time":"2026-09-26T12:00:02.000000000Z","type":"end","span":"#7","parent":"#4","kind":"call","name":"ssh","flags":["show-lines"],"state":"ended","node":"exe0002","host":"exe0002.hpc.example.org","timeout":600,"exit":1,"status":"failed","class":"target","err":"exe0002 (exe0002.hpc.example.org): command exited 1"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":13,"time":"2026-09-26T12:00:02.000000000Z","type":"end","span":"#4","parent":"#2","kind":"target","name":"exe0002","flags":["show-lines"],"state":"ended","node":"exe0002","host":"exe0002.hpc.example.org","status":"failed","class":"target","err":"exe0002 (exe0002.hpc.example.org): command exited 1"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":14,"time":"2026-09-26T12:00:02.000000000Z","type":"run","span":"#5","parent":"#2","kind":"target","name":"exe0003","flags":["show-lines"],"state":"running","node":"exe0003","host":"exe0003.hpc.example.org"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":15,"time":"2026-09-26T12:00:02.000000000Z","type":"start","span":"#8","parent":"#5","kind":"call","name":"ssh","flags":["show-lines"],"state":"running","node":"exe0003","host":"exe0003.hpc.example.org","timeout":600}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":16,"time":"2026-09-26T12:00:03.000000000Z","type":"end","span":"#8","parent":"#5","kind":"call","name":"ssh","flags":["show-lines"],"state":"ended","node":"exe0003","host":"exe0003.hpc.example.org","timeout":600,"exit":0,"status":"ok"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":17,"time":"2026-09-26T12:00:03.000000000Z","type":"end","span":"#5","parent":"#2","kind":"target","name":"exe0003","flags":["show-lines"],"state":"ended","node":"exe0003","host":"exe0003.hpc.example.org","status":"ok"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":18,"time":"2026-09-26T12:00:03.000000000Z","type":"end","span":"#2","parent":"#1","kind":"step","name":"run","flags":["fold","show-lines"],"state":"ended","total":3,"limit":1,"status":"failed","class":"target","err":"1 of 3 failed: exe0002"}
{"v":1,"run":"#run","trace":"4bf92f3577b34da6a3ce929d0e0e4736","seq":19,"time":"2026-09-26T12:00:03.000000000Z","type":"end","span":"#1","kind":"command","name":"exec","state":"ended","status":"failed","class":"target","err":"1 of 3 hosts failed: exe0002"}
`
	if got != want {
		t.Errorf("event log:\n%s\nwant:\n%s", got, want)
	}
}

// logLines returns the lines of an event log, parsed.
func logLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(readLog(t, path)) {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("a line of the event log is not JSON: %q: %v", line, err)
		}
		lines = append(lines, parsed)
	}
	return lines
}

// The event log is written whatever the display, none included, and the
// same events go to both: every span the command started ends in it. The
// text of a line never does, not even while the live tree shows it; the
// log has the line's event, without the text.
func TestTheEventLogIsWrittenWithEveryDisplay(t *testing.T) {
	t.Setenv("TERM", "xterm")
	for _, tc := range []struct {
		mode     string
		terminal bool
		lines    bool
	}{
		{progressNone, false, false},
		{progressPlain, false, false},
		{progressCounter, true, false},
		{progressTTY, true, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c := fakeDisplays(t)
			path := filepath.Join(t.TempDir(), "progress.jsonl")
			opts := harnessOptions{unwatched: true, recorder: &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
				c.draw()
				return exe0002Fails(tg, req)
			}}}
			if tc.terminal {
				opts.streams = onTerminal(false)
			}
			_, err := run(t, opts, "--progress", tc.mode, "--progress-log", path, "exec", "-n", "exe[1-3]", "-y", "--", "uptime")
			wantCode(t, err, exitcode.TargetFailed)
			lines := logLines(t, path)
			if len(lines) == 0 || lines[0]["type"] != "trace" {
				t.Fatalf("the event log does not start with its trace: %v", lines)
			}
			open := map[any]bool{}
			shown := 0
			for _, line := range lines[1:] {
				switch line["type"] {
				case "start":
					open[line["span"]] = true
				case "end":
					delete(open, line["span"])
				case "line":
					shown++
				}
			}
			if len(open) != 0 {
				t.Errorf("%d spans never end in the log", len(open))
			}
			if got := shown > 0; got != tc.lines {
				t.Errorf("%d line events; want some only while the tree asks for lines", shown)
			}
			if log := readLog(t, path); strings.Contains(log, "up 3 days") || strings.Contains(log, "not found") {
				t.Errorf("the log holds what the hosts printed:\n%s", log)
			}
			if first := lines[1]; first["kind"] != "command" || first["name"] != "exec" {
				t.Errorf("the first event is %v, want the command's start", first)
			}
		})
	}
}

// Each command appends to the log, CLUSTERCTL_PROGRESS_LOG's when no flag
// names one, after a line of its own that names its run, the version that
// wrote it and its trace: a trace of its own, unless TRACEPARENT is valid,
// when it continues that one. Every line carries its run, which tells two
// runs apart when they continue one trace. An empty --progress-log writes
// no log, whatever the variable says.
func TestTheEventLogIsAppendedTo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.jsonl")
	t.Setenv(config.EnvProgressLog, path)
	for _, header := range []string{"", "00-" + strings.Repeat("0", 32) + "-00f067aa0ba902b7-01", traceparent, traceparent} {
		t.Setenv("TRACEPARENT", header)
		if _, err := run(t, harnessOptions{unwatched: true}, "node", "fqdn", "-n", "exe1"); err != nil {
			t.Fatalf("node fqdn: %v", err)
		}
	}
	if _, err := run(t, harnessOptions{unwatched: true}, "--progress-log=", "node", "fqdn", "-n", "exe1"); err != nil {
		t.Fatalf("node fqdn: %v", err)
	}
	var traces []map[string]any
	runs := map[any]bool{}
	for _, line := range logLines(t, path) {
		if line["type"] == "trace" {
			traces = append(traces, line)
			runs[line["run"]] = true
		} else if len(traces) == 0 || line["run"] != traces[len(traces)-1]["run"] || line["trace"] != traces[len(traces)-1]["trace"] {
			t.Errorf("%v follows the trace line of another run", line)
		}
	}
	if len(traces) != 4 || len(runs) != 4 {
		t.Fatalf("%d runs in the log with %d names, want 4 of each: %v", len(traces), len(runs), traces)
	}
	if traces[0]["trace"] == traces[1]["trace"] || traces[0]["parent"] != nil || traces[1]["parent"] != nil {
		t.Errorf("runs without a valid TRACEPARENT: %v and %v, want a trace of each's own and no parent", traces[0], traces[1])
	}
	for _, under := range traces[2:] {
		if len(under["run"].(string)) != 16 || under["version"] != version.Get().Version {
			t.Errorf("the run under TRACEPARENT: %v, want 16 hexadecimal digits for its run and the version %q", under, version.Get().Version)
		}
		delete(under, "run")
		delete(under, "version")
		want := map[string]any{"v": 1.0, "type": "trace", "trace": "4bf92f3577b34da6a3ce929d0e0e4736", "parent": "00f067aa0ba902b7", "traceFlags": "01"}
		if fmt.Sprint(under) != fmt.Sprint(want) {
			t.Errorf("the run under TRACEPARENT: %v, want %v", under, want)
		}
	}
}

// A log others could read or write, or one that cannot be opened, is
// refused before anything has run when the flag names it; the file is left
// as it was.
func TestAnEventLogThatCannotBeUsedIsRefused(t *testing.T) {
	dir := t.TempDir()
	wide := filepath.Join(dir, "wide.jsonl")
	if err := os.WriteFile(wide, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, env string
		args      []string
		msg       string
	}{
		{"others can read it", "", []string{"--progress-log", wide}, "--progress-log names a progress log that cannot be used: " + wide + " can be read or written by others than its owner (mode -rw-r--r--); run chmod 600 " + wide},
		{"no such directory", "", []string{"--progress-log", filepath.Join(dir, "missing", "progress.jsonl")}, "no such file or directory"},
		{"a directory", "", []string{"--progress-log", dir}, "is a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvProgressLog, tc.env)
			h, err := run(t, harnessOptions{unwatched: true}, append(tc.args, "exec", "-n", "exe[1-3]", "-y", "--", "uptime")...)
			wantCode(t, err, exitcode.Usage)
			if err != nil && !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error = %v, want it to say %q", err, tc.msg)
			}
			wantNoCalls(t, h)
			if data, _ := os.ReadFile(wide); string(data) != "kept\n" {
				t.Errorf("the refused log now holds %q", data)
			}
		})
	}
}

// A log the variable names that cannot be used fails no command, which
// runs as it would without one; one line on standard error says that no
// log is written, and the file is left as it was.
func TestAnEventLogTheVariableNamesFailsNothing(t *testing.T) {
	dir := t.TempDir()
	wide := filepath.Join(dir, "wide.jsonl")
	if err := os.WriteFile(wide, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"exec", "-n", "exe[1-3]", "-y", "--", "uptime"}
	for _, tc := range []struct{ name, path, msg string }{
		{"others can read it", wide, wide + " can be read or written by others than its owner"},
		{"no such directory", filepath.Join(dir, "missing", "progress.jsonl"), "no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvProgressLog, "")
			quiet, quietErr := run(t, harnessOptions{unwatched: true, recorder: &transport.Recorder{Reply: exe0002Fails}}, args...)
			t.Setenv(config.EnvProgressLog, tc.path)
			h, err := run(t, harnessOptions{unwatched: true, recorder: &transport.Recorder{Reply: exe0002Fails}}, args...)
			if fmt.Sprint(err) != fmt.Sprint(quietErr) {
				t.Errorf("error = %v, want %v", err, quietErr)
			}
			if h.out.String() != quiet.out.String() {
				t.Errorf("standard output = %q, want %q", h.out, quiet.out)
			}
			note, rest, _ := strings.Cut(h.errOut.String(), "\n")
			if !strings.HasPrefix(note, "clusterctl: "+config.EnvProgressLog+" names a progress log that cannot be used: ") ||
				!strings.Contains(note, tc.msg) || !strings.HasSuffix(note, "; no log is written") {
				t.Errorf("first line of standard error = %q, want one that says %q and that no log is written", note, tc.msg)
			}
			if rest != quiet.errOut.String() {
				t.Errorf("standard error after the note = %q, want %q", rest, quiet.errOut)
			}
			if data, _ := os.ReadFile(wide); string(data) != "kept\n" {
				t.Errorf("the log that could not be used now holds %q", data)
			}
		})
	}
}

// A log that cannot be written, on a disk that is full, fails nothing: the
// command ends as it would without it, and one line says the log stops
// short.
func TestAnEventLogThatCannotBeWrittenFailsNothing(t *testing.T) {
	const full = "/dev/full"
	if _, err := os.Stat(full); err != nil {
		t.Skipf("no %s to fill: %v", full, err)
	}
	args := []string{"exec", "-n", "exe[1-3]", "-y", "--", "uptime"}
	quiet, quietErr := run(t, harnessOptions{unwatched: true, recorder: &transport.Recorder{Reply: exe0002Fails}}, args...)
	h, err := run(t, harnessOptions{unwatched: true, recorder: &transport.Recorder{Reply: exe0002Fails}},
		append([]string{"--progress-log", full}, args...)...)
	if fmt.Sprint(err) != fmt.Sprint(quietErr) || h.out.String() != quiet.out.String() {
		t.Errorf("with a full disk, exec printed %q and returned %v, want %q and %v", h.out, err, quiet.out, quietErr)
	}
	want := quiet.errOut.String() + "clusterctl: the progress log /dev/full stops short: write /dev/full: no space left on device\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("standard error:\n%s\nwant:\n%s", got, want)
	}
}
