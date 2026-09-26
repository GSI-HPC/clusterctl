// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// counters stands in for the drawing of the counters the commands of a test
// make: each is drawn only when the test calls draw, on a clock that moves
// a second each time.
type counters struct {
	mu    sync.Mutex
	made  int
	last  *display.Counter
	clock time.Time
}

// fakeCounters has the counters of the commands a test runs drawn by hand.
func fakeCounters(t *testing.T) *counters {
	t.Helper()
	c := &counters{clock: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	saved := startCounter
	startCounter = func(term *display.Terminal) *display.Counter {
		counter := display.NewCounter(term, display.CounterOptions{Now: c.now})
		c.mu.Lock()
		defer c.mu.Unlock()
		c.made++
		c.last = counter
		return counter
	}
	t.Cleanup(func() { startCounter = saved })
	return c
}

func (c *counters) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clock
}

// draw moves the clock on by a second and draws the counter made last.
func (c *counters) draw() {
	c.mu.Lock()
	c.clock = c.clock.Add(time.Second)
	counter := c.last
	c.mu.Unlock()
	if counter != nil {
		counter.Draw()
	}
}

func (c *counters) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.made
}

// onTerminal has standard error, and standard output with out, be a
// terminal 100 columns wide.
func onTerminal(out bool) func(*app.Streams) {
	return func(s *app.Streams) {
		s.ErrIsTTY, s.OutIsTTY = true, out
		s.Size = func() (int, int, error) { return 100, 40, nil }
	}
}

// The counter is drawn where it can be: auto draws it on a terminal that
// is not a dumb one and nowhere else, counter from the flag insists on one,
// and none draws nothing. The variable fails no command: what it asks for
// where it cannot be drawn, or does not name, draws nothing. --progress
// wins over the variable. A Bus the context
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
		drawn    bool
		msg      string
	}{
		{"auto on a terminal", true, "xterm", "", nil, false, exitcode.OK, true, ""},
		{"auto without a terminal", false, "xterm", "", nil, false, exitcode.OK, false, ""},
		{"auto on a dumb terminal", true, "dumb", "", nil, false, exitcode.OK, false, ""},
		{"counter on a terminal", true, "xterm", "", []string{"--progress", "counter"}, false, exitcode.OK, true, ""},
		{"counter without a terminal", false, "xterm", "", []string{"--progress", "counter"}, false, exitcode.Usage, false,
			"--progress asks for a counter, but standard error is not a terminal"},
		{"counter on a dumb terminal", true, "dumb", "", []string{"--progress=counter"}, false, exitcode.Usage, false,
			"TERM is dumb"},
		{"none on a terminal", true, "xterm", "", []string{"--progress", "none"}, false, exitcode.OK, false, ""},
		{"the variable", true, "xterm", "none", nil, false, exitcode.OK, false, ""},
		{"the variable without a terminal", false, "xterm", "counter", nil, false, exitcode.OK, false, ""},
		{"the variable for a counter on a dumb terminal", true, "dumb", "counter", nil, false, exitcode.OK, false, ""},
		{"the flag over the variable", false, "xterm", "counter", []string{"--progress", "none"}, false, exitcode.OK, false, ""},
		{"a value it does not know", true, "xterm", "", []string{"--progress", "tree"}, false, exitcode.Usage, false,
			`--progress is "tree"; it takes one of auto, counter, none`},
		{"a variable it does not know", true, "xterm", "fancy", nil, false, exitcode.OK, false, ""},
		{"a Bus from the context", false, "xterm", "counter", []string{"--progress", "counter"}, true, exitcode.OK, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			t.Setenv(config.EnvProgress, tc.env)
			c := fakeCounters(t)
			opts := harnessOptions{unwatched: !tc.watched}
			if tc.terminal {
				opts.streams = onTerminal(false)
			}
			_, err := run(t, opts, append(tc.args, "node", "fqdn", "-n", "exe1")...)
			wantCode(t, err, tc.code)
			if err != nil && !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error = %v, want it to say %q", err, tc.msg)
			}
			if drawn := c.count() > 0; drawn != tc.drawn {
				t.Errorf("a counter was made: %v, want %v", drawn, tc.drawn)
			}
		})
	}
}

// An agent's commands draw nothing, whatever the variable says, and may not
// ask for a display either.
func TestAnAgentsCommandsDrawNothing(t *testing.T) {
	t.Setenv(config.EnvProgress, "counter")
	c := fakeCounters(t)
	h, cmd := build(t, harnessOptions{unwatched: true, streams: onTerminal(true)}, "node", "fqdn", "-n", "exe1")
	h.root.agent = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("node fqdn under an agent: %v", err)
	}
	if c.count() != 0 || h.errOut.Len() != 0 {
		t.Errorf("an agent's command drew %d counters: %q", c.count(), h.errOut)
	}
}

// With nothing drawn, standard output and standard error carry the bytes
// they would without progress, and so they do while a counter is made but
// not yet drawn, as for a command done within its first second: the
// writers in front of the streams pass on what they are given.
func TestNoDisplayLeavesTheStreamsAsTheyWere(t *testing.T) {
	fakeCounters(t)
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
		"a counter not yet due": {unwatched: true, streams: onTerminal(true)},
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
// table, the error, which report prints once the counter is gone.
func TestTheCounterIsTakenOffBeforeEveryWrite(t *testing.T) {
	c := fakeCounters(t)
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		c.draw()
		if tg.Name == "exe0002" {
			return transport.ExitResult(tg, 1, "", "uptime: not found\n"), nil
		}
		return &transport.Result{Target: tg, Stdout: "up 3 days\n"}, nil
	}}
	tty, err := runOnTerminal(t, harnessOptions{recorder: rec},
		"--fanout", "1", "exec", "-n", "exe[1-3]", "-y", "--", "uptime")
	wantCode(t, err, exitcode.TargetFailed)
	want := `
<erase>run · 0/3 · 1 running · 2 queued · 0:01
<erase>run · 1/3 · 1 running · 1 queued · 0:02
<erase>run · 2/3 · 1 failed · 1 running · 0:03
<erase>exe0001: up 3 days
exe0003: up 3 days
exe0002: exit 1: uptime: not found
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
	c := fakeCounters(t)
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
		append(noSlurm, "-o", "name", "bmc", "power", "on", "--ipmi", "--batch", "2", "-y", "-n", "exe[1-4]")...)
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
	c := fakeCounters(t)
	cluster := newSlurmCluster()
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		c.draw()
		return cluster.reply(tg, req)
	}}
	tty, err := runOnTerminal(t, harnessOptions{recorder: rec, tty: true, stdin: "y\n"},
		"slurm", "node", "drain", "ticket 4711: failing DIMM", "-n", "exe0007")
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
`
	if got := tty.String(); got != want {
		t.Errorf("terminal:\n%s\nwant:\n%s", got, want)
	}
}

// scp draws its meter on the line the counter has, so it is left off while
// a counter is drawn, even for one transfer.
func TestTheCounterTakesTheLineFromScpsMeter(t *testing.T) {
	fakeCounters(t)
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
