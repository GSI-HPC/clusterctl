// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// displays stands in for the displays of the commands a test runs: each is
// drawn only when the test calls draw, on a clock that moves a second each
// time, which the displays' Bus reads too.
type displays struct {
	mu    sync.Mutex
	modes []string
	last  interface{ Draw() }
	clock time.Time
}

// fakeDisplays has the displays of the commands a test runs drawn by hand.
func fakeDisplays(t *testing.T) *displays {
	t.Helper()
	c := &displays{clock: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	saved, savedClock := startDisplay, displayClock
	startDisplay = func(mode string, term *display.Terminal, interrupted <-chan struct{}) renderer {
		var shown interface {
			renderer
			Draw()
		}
		switch mode {
		case progressPlain:
			shown = display.NewPlain(term, display.PlainOptions{Now: c.now})
		case progressTTY:
			shown = display.NewTree(term, display.TreeOptions{Now: c.now, Interrupted: interrupted})
		default:
			shown = display.NewCounter(term, display.CounterOptions{Now: c.now})
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.modes = append(c.modes, mode)
		c.last = shown
		return shown
	}
	displayClock = c.now
	t.Cleanup(func() { startDisplay, displayClock = saved, savedClock })
	return c
}

func (c *displays) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clock
}

// draw moves the clock on by a second and draws the display made last.
func (c *displays) draw() {
	c.mu.Lock()
	c.clock = c.clock.Add(time.Second)
	shown := c.last
	c.mu.Unlock()
	if shown != nil {
		shown.Draw()
	}
}

func (c *displays) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.modes)
}

// shown returns the modes of the displays made, in the order they were.
func (c *displays) shown() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.modes, ",")
}

// onTerminal has standard error, and standard output with out, be a
// terminal 100 columns wide.
func onTerminal(out bool) func(*app.Streams) {
	return func(s *app.Streams) {
		s.ErrIsTTY, s.OutIsTTY = true, out
		s.Size = func() (int, int, error) { return 100, 40, nil }
	}
}

// The live tree is drawn where it can be: auto draws it on a terminal that
// is not a dumb one and nowhere else, tty and counter from the flag insist
// on one, plain lines need none, and none draws nothing. The variable fails
// no command: what it asks for where it cannot be drawn, or does not name,
// draws nothing. --progress wins over the variable. A Bus the context
// brought, as a test's or an MCP call's, draws nothing of its own, whatever
// the flag and the variable say.
func TestTheProgressDisplayIsDrawnWhereItCan(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		term     string
		env      string
		args     []string
		watched  bool
		code     int
		shown    string
		msg      string
	}{
		{"auto on a terminal", true, "xterm", "", nil, false, exitcode.OK, progressTTY, ""},
		{"auto without a terminal", false, "xterm", "", nil, false, exitcode.OK, "", ""},
		{"auto on a dumb terminal", true, "dumb", "", nil, false, exitcode.OK, "", ""},
		{"tty on a terminal", true, "xterm", "", []string{"--progress", "tty"}, false, exitcode.OK, progressTTY, ""},
		{"tty without a terminal", false, "xterm", "", []string{"--progress", "tty"}, false, exitcode.Usage, "",
			"--progress asks for a live tree, but standard error is not a terminal"},
		{"tty on a dumb terminal", true, "dumb", "", []string{"--progress=tty"}, false, exitcode.Usage, "",
			"TERM is dumb"},
		{"counter on a terminal", true, "xterm", "", []string{"--progress", "counter"}, false, exitcode.OK, progressCounter, ""},
		{"counter without a terminal", false, "xterm", "", []string{"--progress", "counter"}, false, exitcode.Usage, "",
			"--progress asks for a counter, but standard error is not a terminal"},
		{"counter on a dumb terminal", true, "dumb", "", []string{"--progress=counter"}, false, exitcode.Usage, "",
			"TERM is dumb"},
		{"plain without a terminal", false, "xterm", "", []string{"--progress", "plain"}, false, exitcode.OK, progressPlain, ""},
		{"plain on a dumb terminal", true, "dumb", "", []string{"--progress=plain"}, false, exitcode.OK, progressPlain, ""},
		{"none on a terminal", true, "xterm", "", []string{"--progress", "none"}, false, exitcode.OK, "", ""},
		{"the variable", true, "xterm", "none", nil, false, exitcode.OK, "", ""},
		{"the variable for a counter", true, "xterm", "counter", nil, false, exitcode.OK, progressCounter, ""},
		{"the variable without a terminal", false, "xterm", "tty", nil, false, exitcode.OK, "", ""},
		{"the variable for a counter on a dumb terminal", true, "dumb", "counter", nil, false, exitcode.OK, "", ""},
		{"the variable for plain lines without a terminal", false, "xterm", "plain", nil, false, exitcode.OK, progressPlain, ""},
		{"the flag over the variable", false, "xterm", "counter", []string{"--progress", "none"}, false, exitcode.OK, "", ""},
		{"a value it does not know", true, "xterm", "", []string{"--progress", "tree"}, false, exitcode.Usage, "",
			`--progress is "tree"; it takes one of auto, tty, counter, plain, none`},
		{"a variable it does not know", true, "xterm", "fancy", nil, false, exitcode.OK, "", ""},
		{"a Bus from the context", false, "xterm", "tty", []string{"--progress", "tty"}, true, exitcode.OK, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			t.Setenv(config.EnvProgress, tc.env)
			c := fakeDisplays(t)
			opts := harnessOptions{unwatched: !tc.watched}
			if tc.terminal {
				opts.streams = onTerminal(false)
			}
			_, err := run(t, opts, append(tc.args, "node", "fqdn", "-n", "exe1")...)
			wantCode(t, err, tc.code)
			if err != nil && !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error = %v, want it to say %q", err, tc.msg)
			}
			if got := c.shown(); got != tc.shown {
				t.Errorf("displays made: %q, want %q", got, tc.shown)
			}
		})
	}
}

// auto draws nothing while standard output goes into a pipe, whose reader
// writes to the terminal the tree would be drawn on; the tree asked for is
// drawn all the same, and so are plain lines.
func TestAutoDrawsNothingWhileTheOutputGoesIntoAPipe(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv(config.EnvProgress, "")
	intoAPipe := func(s *app.Streams) {
		onTerminal(false)(s)
		s.OutIsPipe = true
	}
	for _, tc := range []struct {
		name  string
		args  []string
		shown string
	}{
		{"auto", nil, ""},
		{"tty", []string{"--progress", "tty"}, progressTTY},
		{"plain", []string{"--progress", "plain"}, progressPlain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeDisplays(t)
			if _, err := run(t, harnessOptions{unwatched: true, streams: intoAPipe}, append(tc.args, "node", "fqdn", "-n", "exe1")...); err != nil {
				t.Fatal(err)
			}
			if got := c.shown(); got != tc.shown {
				t.Errorf("displays made: %q, want %q", got, tc.shown)
			}
		})
	}
}

// A value of the variable that names no display is said once on standard
// error, and the command runs as it would with none; one that asks for a
// display where it cannot be drawn is not said at all, since the cron job
// or the CI step that inherited it never asked. Standard output is left
// alone either way.
func TestTheVariableFailsNoCommand(t *testing.T) {
	for _, tc := range []struct {
		name, env, errOut string
	}{
		{"a value it does not know", "plian",
			"clusterctl: " + config.EnvProgress + ` is "plian"; it takes one of auto, tty, counter, plain, none; no progress is shown` + "\n"},
		{"a tree without a terminal", "tty", ""},
		{"a counter without a terminal", "counter", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvProgress, tc.env)
			c := fakeDisplays(t)
			quiet, err := run(t, harnessOptions{unwatched: true}, "--progress=none", "node", "fqdn", "-n", "exe1")
			if err != nil {
				t.Fatalf("node fqdn with --progress=none: %v", err)
			}
			h, err := run(t, harnessOptions{unwatched: true}, "node", "fqdn", "-n", "exe1")
			if err != nil {
				t.Fatalf("node fqdn with %s=%s: %v", config.EnvProgress, tc.env, err)
			}
			if c.count() != 0 {
				t.Errorf("%d displays made, want none", c.count())
			}
			if got := h.errOut.String(); got != quiet.errOut.String()+tc.errOut {
				t.Errorf("standard error = %q, want %q", got, quiet.errOut.String()+tc.errOut)
			}
			if h.out.String() != quiet.out.String() {
				t.Errorf("standard output = %q, want %q", h.out, quiet.out)
			}
		})
	}
}

// The live tree draws its marks in UTF-8 only where the locale says the
// terminal shows it, as the C library reads the locale, and in ASCII
// elsewhere.
func TestTheTreeDrawsInTheLocalesCharacters(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"no locale", nil, false},
		{"a UTF-8 language", map[string]string{"LANG": "en_US.UTF-8"}, true},
		{"the C locale in UTF-8", map[string]string{"LANG": "C.utf8"}, true},
		{"a language in Latin-1", map[string]string{"LANG": "de_DE.ISO-8859-1"}, false},
		{"the character type over the language", map[string]string{"LANG": "en_US.UTF-8", "LC_CTYPE": "C"}, false},
		{"all over the rest", map[string]string{"LANG": "C", "LC_CTYPE": "C", "LC_ALL": "de_DE.UTF-8"}, true},
		{"an empty variable is none", map[string]string{"LC_ALL": "", "LANG": "en_GB.UTF-8"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := utf8Locale(func(name string) string { return tc.env[name] }); got != tc.want {
				t.Errorf("utf8Locale(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// An agent's commands draw nothing, whatever the variable says, and may not
// ask for a display either.
func TestAnAgentsCommandsDrawNothing(t *testing.T) {
	t.Setenv(config.EnvProgress, "counter")
	c := fakeDisplays(t)
	h, cmd := build(t, harnessOptions{unwatched: true, streams: onTerminal(true)}, "node", "fqdn", "-n", "exe1")
	h.root.agent = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("node fqdn under an agent: %v", err)
	}
	if c.count() != 0 || h.errOut.Len() != 0 {
		t.Errorf("an agent's command made %d displays: %q", c.count(), h.errOut)
	}
}

// With nothing drawn, standard output and standard error carry the bytes
// they would without progress, and so they do while the tree is made but
// not yet drawn, as for a command done within its first second: the
// writers in front of the streams pass on what they are given, and the
// lines of the steps that finished are never written. The one line the
// display adds is the summary it leaves behind, since a target failed.
func TestNoDisplayLeavesTheStreamsAsTheyWere(t *testing.T) {
	fakeDisplays(t)
	rec := func() *transport.Recorder {
		return &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
			if tg.Name == "exe0002" {
				return transport.ExitResult(tg, 1, "", "uptime: not found\n"), nil
			}
			return &transport.Result{Target: tg, Stdout: "up 3 days\n"}, nil
		}}
	}
	args := []string{"exec", "-n", "exe[1-3]", "-y", "--", "uptime"}
	type result struct {
		out, errOut string
		code        int
	}
	runs := map[string]result{}
	for name, opts := range map[string]harnessOptions{
		"no terminal":           {unwatched: true},
		"none on a terminal":    {unwatched: true, streams: onTerminal(true)},
		"a display not yet due": {unwatched: true, streams: onTerminal(true)},
	} {
		opts.recorder = rec()
		line := args
		if name == "none on a terminal" {
			line = append([]string{"--progress", "none"}, args...)
		}
		h, err := run(t, opts, line...)
		runs[name] = result{h.out.String(), h.errOut.String(), exitcode.From(err)}
	}
	want := runs["no terminal"]
	if want.out == "" || want.errOut == "" || want.code != exitcode.TargetFailed {
		t.Fatalf("exec printed %q and %q and exited %d; the test wants output on both streams and a failure", want.out, want.errOut, want.code)
	}
	for name, got := range runs {
		if got != want {
			t.Errorf("%s:\n%+v\nwant:\n%+v", name, got, want)
		}
	}
}

// terminal is one terminal that standard output and standard error both
// show on, the way they do for a command run at a prompt.
type terminal struct {
	mu sync.Mutex
	b  strings.Builder
}

func (t *terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}

// String returns what the terminal received, each erasing of the counter's
// line shown as "<erase>" at the start of a line of its own.
func (t *terminal) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ReplaceAll(t.b.String(), "\r\x1b[2K", "\n<erase>")
}

// runOnTerminal runs a command line with standard output and standard
// error on one terminal, 100 columns wide, and prints its error there the
// way the command line does.
func runOnTerminal(t *testing.T, opts harnessOptions, args ...string) (*terminal, error) {
	t.Helper()
	tty := &terminal{}
	opts.unwatched = true
	opts.streams = func(s *app.Streams) {
		s.Out, s.Err = tty, tty
		onTerminal(true)(s)
	}
	_, cmd := build(t, opts, args...)
	cmd.SetOut(tty)
	cmd.SetErr(tty)
	err := cmd.Execute()
	if err != nil {
		report(context.Background(), app.Streams{Err: tty}, err)
	}
	return tty, err
}

// The counter is drawn on standard error, and taken off before anything
// else is written on the terminal: exec's output and its failures, the
// table, the summary it leaves behind, and the error, which report prints
// once the counter is gone.
func TestTheCounterIsTakenOffBeforeEveryWrite(t *testing.T) {
	c := fakeDisplays(t)
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		c.draw()
		if tg.Name == "exe0002" {
			return transport.ExitResult(tg, 1, "", "uptime: not found\n"), nil
		}
		return &transport.Result{Target: tg, Stdout: "up 3 days\n"}, nil
	}}
	tty, err := runOnTerminal(t, harnessOptions{recorder: rec},
		"--progress", "counter", "--fanout", "1", "exec", "-n", "exe[1-3]", "-y", "--", "uptime")
	wantCode(t, err, exitcode.TargetFailed)
	want := `
<erase>run · 0/3 · 1 running · 2 queued · 0:01
<erase>run · 1/3 · 1 running · 1 queued · 0:02
<erase>run · 2/3 · 1 failed · 1 running · 0:03
<erase>exe0001: up 3 days
exe0003: up 3 days
exe0002: exit 1: uptime: not found
clusterctl: exec: failed in 3.0s: 2 ok, 1 failed
clusterctl: 1 of 3 hosts failed: exe0002
`
	if got := tty.String(); got != want {
		t.Errorf("terminal:\n%s\nwant:\n%s", got, want)
	}
}

// A power-on in batches is counted as a whole, with the batch under way
// and the pause between two; the notes it writes between the batches take
// the counter off, and it comes back below them.
func TestTheCounterOfAPowerOnInBatches(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	c := fakeDisplays(t)
	fakeStagger(t, func(time.Duration) bool {
		c.draw()
		return false
	})
	answer := ipmiAnswer(func(bmc string) string {
		if strings.HasPrefix(bmc, "exe0004.") {
			return "connection timeout"
		}
		return "ok"
	})
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		c.draw()
		return answer(tg, req)
	}}
	tty, err := runOnTerminal(t, harnessOptions{recorder: rec},
		append(noSlurm, "--progress", "counter", "-o", "name", "bmc", "power", "on", "--ipmi", "--batch", "2", "-y", "-n", "exe[1-4]")...)
	wantCode(t, err, exitcode.TargetFailed)
	want := `powering on exe[0001-0002] (1 of 2)

<erase>power on · batch 1/2 · 0/4 · 2 running · 2 queued · 0:01
<erase>waiting 5s before the next batch

<erase>power on · batch 1/2 · 2/4 · 2 queued · waiting · 0:02
<erase>powering on exe[0003-0004] (2 of 2)

<erase>power on · batch 2/2 · 2/4 · 2 running · 0:03
<erase>exe0001
exe0002
exe0003
exe0004
clusterctl: bmc power: failed in 3.0s: 3 ok, 1 failed
clusterctl: 1 of 4 service processors failed
`
	if got := tty.String(); got != want {
		t.Errorf("terminal:\n%s\nwant:\n%s", got, want)
	}
}

// The confirmation is never drawn over: the counter leaves the terminal
// before the preview and the question, and comes back once the question
// has been answered.
func TestTheCounterLeavesTheQuestionAlone(t *testing.T) {
	c := fakeDisplays(t)
	cluster := newSlurmCluster()
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		c.draw()
		return cluster.reply(tg, req)
	}}
	tty, err := runOnTerminal(t, harnessOptions{recorder: rec, tty: true, stdin: "y\n"},
		"--progress", "counter", "slurm", "node", "drain", "ticket 4711: failing DIMM", "-n", "exe0007")
	if err != nil {
		t.Fatalf("slurm node drain failed: %v", err)
	}
	// The answer is not echoed here, as a terminal would echo it.
	want := `
<erase>slurm node drain · 0:01
<erase>About to drain 1 host: exe0007
  reason: "ticket 4711: failing DIMM"
Continue? [y/N] 
<erase>slurm node drain · 0:02
<erase>drained exe0007
clusterctl: slurm node drain: done in 2.0s
`
	if got := tty.String(); got != want {
		t.Errorf("terminal:\n%s\nwant:\n%s", got, want)
	}
}

// typing is standard input typed at a terminal: each read draws the
// display first, as a second passing while the administrator types would.
type typing struct {
	c    *displays
	text []string
}

func (in *typing) Read(p []byte) (int, error) {
	if len(in.text) == 0 {
		return 0, io.EOF
	}
	in.c.draw()
	n := copy(p, in.text[0])
	in.text = in.text[1:]
	return n, nil
}

// exec --stdin typed at a terminal is read with the counter off it, as a
// question is: what is typed is not drawn over.
func TestTheCounterLeavesTypedInputAlone(t *testing.T) {
	c := fakeDisplays(t)
	c.draw()
	in := &typing{c: c, text: []string{"hello\n", "world\n"}}
	tty, err := runOnTerminal(t, harnessOptions{recorder: &transport.Recorder{}, tty: true, in: in},
		"--progress", "counter", "exec", "--stdin", "-y", "-n", "exe1", "--", "tee", "/tmp/x")
	if err != nil {
		t.Fatalf("exec --stdin failed: %v", err)
	}
	if got := tty.String(); strings.Contains(got, "exec ·") {
		t.Errorf("the counter was drawn while the input was typed:\n%s", got)
	}
}

// runOnScreen runs a command line with standard output and standard error
// on screen, a terminal 100 columns wide and 40 rows high that echoes what
// is read from standard input, and prints its error there the way the
// command line does.
func runOnScreen(t *testing.T, screen *progresstest.Screen, opts harnessOptions, args ...string) error {
	t.Helper()
	opts.unwatched = true
	opts.streams = func(s *app.Streams) {
		s.In = io.TeeReader(s.In, screen)
		s.Out, s.Err = screen, screen
		onTerminal(true)(s)
	}
	_, cmd := build(t, opts, args...)
	cmd.SetOut(screen)
	cmd.SetErr(screen)
	err := cmd.Execute()
	if err != nil {
		report(context.Background(), app.Streams{Err: screen}, err)
	}
	return err
}

// On a terminal the live tree is drawn by default: the command, the step
// with how its targets stand, and the one running with the request it waits
// for, against its bound. What the command prints lands above it, and once
// the command is over the tree is gone: the step's line, with the target
// that failed, the command's output, the summary and the error are what is
// left.
func TestTheTreeIsDrawnByDefault(t *testing.T) {
	c := fakeDisplays(t)
	screen := &progresstest.Screen{Width: 100}
	var frames []string
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		c.draw()
		frames = append(frames, screen.String())
		if tg.Name == "exe0002" {
			return transport.ExitResult(tg, 1, "", "uptime: not found\n"), nil
		}
		return &transport.Result{Target: tg, Stdout: "up 3 days\n"}, nil
	}}
	err := runOnScreen(t, screen, harnessOptions{recorder: rec}, "--fanout", "1", "exec", "-n", "exe[1-3]", "-y", "--", "uptime")
	wantCode(t, err, exitcode.TargetFailed)
	want := []string{`
exec · 0:01
  run  0/3 · 1 running · 2 queued
    ▸ exe0001  1s/10m  ssh
`, `
exec · 0:02
  run  1/3 · 1 running · 1 queued
    ▸ exe0002  1s/10m  ssh
    ✓ exe0001
`, `
exec · 0:03
  run  2/3 · 1 failed · 1 running
    ✗ exe0002  target: {} ({}): command exited 1
    ▸ exe0003  1s/10m  ssh
    ✓ exe0001
`}
	for i, frame := range frames {
		if i < len(want) && frame != want[i][1:] {
			t.Errorf("frame %d:\n%s\nwant:\n%s", i+1, frame, want[i][1:])
		}
	}
	if len(frames) != len(want) {
		t.Errorf("%d frames, want %d", len(frames), len(want))
	}
	end := `
✗ run  3.0s  2 ok, 1 failed
  ✗ exe0002  target: {} ({}): command exited 1
exe0001: up 3 days
exe0003: up 3 days
exe0002: exit 1: uptime: not found
clusterctl: exec: failed in 3.0s: 2 ok, 1 failed
clusterctl: 1 of 3 hosts failed: exe0002
`
	if got := screen.String(); got != end[1:] {
		t.Errorf("terminal:\n%s\nwant:\n%s", got, end[1:])
	}
}

// The confirmation is never drawn over: the tree leaves the terminal before
// the preview and the question, and comes back below them once the question
// has been answered. A call made for the command itself, the drain on the
// Slurm controller, has a row of its own.
func TestTheTreeLeavesTheQuestionAlone(t *testing.T) {
	c := fakeDisplays(t)
	cluster := newSlurmCluster()
	screen := &progresstest.Screen{Width: 100}
	var frames []string
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		c.draw()
		frames = append(frames, screen.String())
		return cluster.reply(tg, req)
	}}
	err := runOnScreen(t, screen, harnessOptions{recorder: rec, tty: true, stdin: "y\n"},
		"slurm", "node", "drain", "ticket 4711: failing DIMM", "-n", "exe0007")
	if err != nil {
		t.Fatalf("slurm node drain failed: %v", err)
	}
	want := []string{`
slurm node drain · 0:01
  ssh login  1s/10m
`, `
About to drain 1 host: exe0007
  reason: "ticket 4711: failing DIMM"
Continue? [y/N] y
slurm node drain · 0:02
  ssh login  1s/10m
`}
	for i, frame := range frames {
		if i < len(want) && frame != want[i][1:] {
			t.Errorf("frame %d:\n%s\nwant:\n%s", i+1, frame, want[i][1:])
		}
	}
	end := `
About to drain 1 host: exe0007
  reason: "ticket 4711: failing DIMM"
Continue? [y/N] y
drained exe0007
clusterctl: slurm node drain: done in 2.0s
`
	if got := screen.String(); got != end[1:] {
		t.Errorf("terminal:\n%s\nwant:\n%s", got, end[1:])
	}
}

// scp draws its meter on the line the counter has, so it is left off while
// a counter is drawn, even for one transfer.
func TestTheCounterTakesTheLineFromScpsMeter(t *testing.T) {
	fakeDisplays(t)
	binary, _ := fakeScp(t, `echo "hosts 100% 1024 1.0MB/s 00:00"`)
	for _, tc := range []struct {
		progress string
		meters   int
	}{{"none", 1}, {"counter", 0}} {
		tty, err := runOnTerminal(t, harnessOptions{},
			"--progress", tc.progress, "--set", "ssh.scpBinary="+binary, "-y", "copy", "-n", "exe1", "/etc/hosts", "/etc/hosts")
		if err != nil {
			t.Fatalf("copy failed: %v", err)
		}
		if got := strings.Count(tty.String(), "hosts 100%"); got != tc.meters {
			t.Errorf("--progress %s: %d meters on the terminal, want %d:\n%s", tc.progress, got, tc.meters, tty)
		}
	}
}

// runPlain runs a command line with --progress plain, standard error not a
// terminal, and prints its error on standard error the way the command line
// does.
func runPlain(t *testing.T, opts harnessOptions, args ...string) (*harness, error) {
	t.Helper()
	opts.unwatched = true
	h, cmd := build(t, opts, append([]string{"--progress", "plain"}, args...)...)
	err := cmd.Execute()
	if err != nil {
		report(context.Background(), app.Streams{Err: h.errOut}, err)
	}
	return h, err
}

// Plain lines need no terminal: a fan-out says when its step starts, each
// target that fails, once, and how the step ended, and leaves the summary
// before the error. They go to standard error, ahead of what the command
// writes there after them, and standard output is what it is without them.
func TestPlainLinesOfAFanOut(t *testing.T) {
	c := fakeDisplays(t)
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		c.draw()
		if tg.Name == "exe0002" {
			return transport.ExitResult(tg, 1, "", "uptime: not found\n"), nil
		}
		return &transport.Result{Target: tg, Stdout: "up 3 days\n"}, nil
	}}
	h, err := runPlain(t, harnessOptions{recorder: rec}, "--fanout", "1", "exec", "-n", "exe[1-3]", "-y", "--", "uptime")
	wantCode(t, err, exitcode.TargetFailed)
	want := `[0:00] exec › run: start, 3 hosts, 1 at a time
[0:02] exec › run › exe0002 failed (target): exe0002 (exe0002.hpc.example.org): command exited 1
[0:03] exec › run: failed in 3.0s: 2 ok, 1 failed
exe0002: exit 1: uptime: not found
clusterctl: exec: failed in 3.0s: 2 ok, 1 failed
clusterctl: 1 of 3 hosts failed: exe0002
`
	if got := h.errOut.String(); got != want {
		t.Errorf("standard error:\n%s\nwant:\n%s", got, want)
	}
	if got, want := h.out.String(), "exe0001: up 3 days\nexe0003: up 3 days\n"; got != want {
		t.Errorf("standard output:\n%s\nwant:\n%s", got, want)
	}
}

// A power-on in batches says when each batch starts and ends, and the
// pause between two, around the notes the command writes itself.
func TestPlainLinesOfAPowerOnInBatches(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	c := fakeDisplays(t)
	fakeStagger(t, func(time.Duration) bool {
		c.draw()
		return false
	})
	answer := ipmiAnswer(func(bmc string) string {
		if strings.HasPrefix(bmc, "exe0004.") {
			return "connection timeout"
		}
		return "ok"
	})
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		c.draw()
		return answer(tg, req)
	}}
	h, err := runPlain(t, harnessOptions{recorder: rec},
		append(noSlurm, "-o", "name", "bmc", "power", "on", "--ipmi", "--batch", "2", "-y", "-n", "exe[1-4]")...)
	wantCode(t, err, exitcode.TargetFailed)
	want := `[0:00] bmc power › power on: start, 4 hosts
powering on exe[0001-0002] (1 of 2)
[0:00] bmc power › power on › batch 1/2: start, 2 hosts
[0:01] bmc power › power on › batch 1/2: done in 1.0s: 2 ok
waiting 5s before the next batch
[0:01] bmc power › power on › stagger: waiting 5s
powering on exe[0003-0004] (2 of 2)
[0:02] bmc power › power on › batch 2/2: start, 2 hosts
[0:03] bmc power › power on › batch 2/2 › exe0004 failed (target): connection timeout
[0:03] bmc power › power on › batch 2/2: failed in 1.0s: 1 ok, 1 failed
[0:03] bmc power › power on: failed in 3.0s: 3 ok, 1 failed
clusterctl: bmc power: failed in 3.0s: 3 ok, 1 failed
clusterctl: 1 of 4 service processors failed
`
	if got := h.errOut.String(); got != want {
		t.Errorf("standard error:\n%s\nwant:\n%s", got, want)
	}
}

// A reinstall that fails: the steps that count nothing say only how they
// ended, the lookups before the question nothing, and the summary counts
// each node once and leaves why it failed to the error.
func TestPlainLinesOfAFailure(t *testing.T) {
	fakeDisplays(t)
	h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
	h.bmcs.down["exe0002"] = true
	out, err := h.run(t, harnessOptions{unwatched: true}, "--progress", "plain", "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
	wantCode(t, err, exitcode.Transport)
	report(context.Background(), app.Streams{Err: out.errOut}, err)
	want := `no certificate is recorded for exe0001.mgmt.hpc.example.org yet; the one it presents now will be recorded and trusted from then on
no certificate is recorded for exe0002.mgmt.hpc.example.org yet; the one it presents now will be recorded and trusted from then on
no certificate is recorded for exe0003.mgmt.hpc.example.org yet; the one it presents now will be recorded and trusted from then on
[0:00] provision reinstall › configuring the network boot: start
[0:00] provision reinstall › configuring the network boot: done in 0.0s
[0:00] provision reinstall › setting the machines to boot from the network once: start, 3 hosts
[0:00] provision reinstall › setting the machines to boot from the network once › exe0002 failed (transport): exe0002.mgmt.hpc.example.org: dial tcp: connection refused
[0:00] provision reinstall › setting the machines to boot from the network once: failed in 0.0s: 2 ok, 1 failed
[0:00] provision reinstall › disarming: start
[0:00] provision reinstall › disarming › clearing the boot overrides: start, 2 hosts
[0:00] provision reinstall › disarming › clearing the boot overrides: done in 0.0s: 2 ok
[0:00] provision reinstall › disarming: done in 0.0s
clusterctl: setting the machines to boot from the network once failed: exe0002: exe0002.mgmt.hpc.example.org: dial tcp: connection refused; the boot links and boot overrides of exe[0001-0003] were removed again
`
	if got := out.errOut.String(); got != want {
		t.Errorf("standard error:\n%s\nwant:\n%s", got, want)
	}
}
