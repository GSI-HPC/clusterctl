// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// screen is a terminal a test reads back.
type screen struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns what was written, with each erasing of the line shown as
// "<erase>" and each line drawn after it on a line of its own.
func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.ReplaceAll(s.b.String(), "\r\x1b[2K", "\n<erase>")
}

// clock is a clock a test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fixture is a counter on a screen, fed by a Bus whose events are checked
// when the test ends, for a command that started when the counter was
// made.
type fixture struct {
	ctx     context.Context
	screen  *screen
	term    *display.Terminal
	counter *display.Counter
	clock   *clock
	command *progress.Span
}

func newFixture(t *testing.T, command string, size func() (int, int, error)) *fixture {
	t.Helper()
	f := &fixture{screen: &screen{}, clock: &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}}
	f.term = display.NewTerminal(f.screen, size)
	f.counter = display.NewCounter(f.term, display.CounterOptions{Now: f.clock.Now})
	capture := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{capture, f.counter}, Now: f.clock.Now})
	t.Cleanup(func() {
		bus.Close()
		f.counter.Close()
		progresstest.Check(t, capture.Events())
	})
	f.ctx, f.command = progress.Start(progress.WithBus(context.Background(), bus), progress.KindCommand, command)
	return f
}

// draw moves the clock on by d and draws a frame.
func (f *fixture) draw(d time.Duration) {
	f.clock.Add(d)
	f.counter.Draw()
}

// frames returns the lines the counter drew, in order.
func (f *fixture) frames() []string {
	var out []string
	for _, line := range strings.Split(f.screen.String(), "\n<erase>") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func check(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("frames:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A command done within a second never shows a counter.
func TestTheCounterWaitsASecond(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "node hw", nil)
	f.draw(0)
	f.draw(999 * time.Millisecond)
	if got := f.screen.String(); got != "" {
		t.Fatalf("the counter was drawn within its first second: %q", got)
	}
	f.draw(time.Millisecond)
	check(t, f.frames(), "node hw · 0:01")
}

// A fan-out is counted as its targets end, however they end, with those
// that fail, run and wait said as well.
func TestTheCounterCountsAFanOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "exec", nil)
	f.draw(time.Second)
	nodes := []string{"exe1", "exe2", "exe3", "exe4"}
	fanout.Map(f.ctx, nodes, fanout.Options[string]{Step: "run", Limit: 1}, func(_ context.Context, node string) (struct{}, error) {
		f.draw(time.Second)
		if node == "exe2" {
			return struct{}{}, errors.New("exe2: command exited 1")
		}
		return struct{}{}, nil
	})
	f.draw(time.Second)
	check(t, f.frames(),
		"exec · 0:01",
		"run · 0/4 · 1 running · 3 queued · 0:02",
		"run · 1/4 · 1 running · 2 queued · 0:03",
		"run · 2/4 · 1 failed · 1 running · 1 queued · 0:04",
		"run · 3/4 · 1 failed · 1 running · 0:05",
		"exec · 0:06",
	)
}

// A wide fan-out reads the way the manual shows it.
func TestTheCounterOfAWideFanOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "provision reinstall", func() (int, int, error) { return 120, 40, nil })
	ctx, step := progress.Start(f.ctx, progress.KindStep, "reset the machines",
		progress.WithFlags(progress.Fold), progress.Total(480), progress.Limit(8))
	targets := make([]*progress.Span, 480)
	for i := range targets {
		node := fmt.Sprintf("exe%d", i+1)
		_, targets[i] = progress.Start(ctx, progress.KindTarget, node, progress.Queued(), progress.Node(node))
	}
	end := func(i int) {
		var err error
		if i == 40 {
			err = errors.New("no answer")
		}
		targets[i].End(err)
	}
	// Eight at a time: each target that ends makes room for the next.
	for i := range 320 {
		if i >= 8 {
			end(i - 8)
		}
		targets[i].Run()
	}
	f.draw(4*time.Minute + 12*time.Second)
	for i := 312; i < 480; i++ {
		if i < 320 {
			end(i)
			continue
		}
		targets[i].Run()
		end(i)
	}
	step.End(errors.New("1 of 480 failed"))
	check(t, f.frames(), "reset the machines · 312/480 · 1 failed · 8 running · 160 queued · 4:12")
}

// A power-on in batches is counted as a whole, with the batch under way,
// the pause between two, and the batches left out after one failed.
func TestTheCounterCountsAPowerOnInBatches(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "bmc power on", nil)
	nodes, err := nodeset.Parse("exe[1-6]")
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Add(time.Second)
	fanout.Batches(f.ctx, nodes, fanout.BatchOptions{
		Step:  "power on",
		Size:  2,
		Pause: 30 * time.Second,
		After: func(d time.Duration) <-chan time.Time {
			f.draw(time.Second)
			f.clock.Add(d)
			ready := make(chan time.Time, 1)
			ready <- f.clock.Now()
			return ready
		},
	}, func(ctx context.Context, batch *nodeset.NodeSet) error {
		f.draw(time.Second)
		names := batch.Expand()
		spans := make([]*progress.Span, len(names))
		for i, node := range names {
			_, spans[i] = progress.Start(ctx, progress.KindTarget, node, progress.Queued(), progress.Node(node))
		}
		var failed error
		for i, node := range names {
			spans[i].Run()
			f.draw(time.Second)
			if node == "exe4" {
				failed = errors.New("exe4: no answer")
			}
			spans[i].End(failed)
		}
		return failed
	})
	f.draw(time.Second)
	check(t, f.frames(),
		"power on · batch 1/3 · 0/6 · 4 queued · 0:02",
		"power on · batch 1/3 · 0/6 · 1 running · 5 queued · 0:03",
		"power on · batch 1/3 · 1/6 · 1 running · 4 queued · 0:04",
		"power on · batch 1/3 · 2/6 · 4 queued · waiting · 0:05",
		"power on · batch 2/3 · 2/6 · 2 queued · 0:36",
		"power on · batch 2/3 · 2/6 · 1 running · 3 queued · 0:37",
		"power on · batch 2/3 · 3/6 · 1 running · 2 queued · 0:38",
		"bmc power on · 0:39",
	)
}

// Two steps under way side by side each have a segment, and the one that
// ends first leaves the line.
func TestTheCounterShowsTwoStepsSideBySide(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "provision status", func() (int, int, error) { return 160, 40, nil })
	start := func(name string, n int) (*progress.Span, []*progress.Span) {
		ctx, step := progress.Start(f.ctx, progress.KindStep, name, progress.WithFlags(progress.Fold), progress.Total(n))
		targets := make([]*progress.Span, n)
		for i := range targets {
			node := fmt.Sprintf("exe%d", i+1)
			_, targets[i] = progress.Start(ctx, progress.KindTarget, node, progress.Queued(), progress.Node(node))
		}
		return step, targets
	}
	power, bmcs := start("read the power state", 3)
	ssh, nodes := start("run", 3)
	bmcs[0].Run()
	nodes[0].Run()
	nodes[1].Run()
	f.draw(2 * time.Second)
	bmcs[0].End(nil)
	bmcs[1].Run()
	nodes[0].End(errors.New("exe1: connection refused"))
	f.draw(time.Second)
	for _, bmc := range bmcs[1:] {
		bmc.Run()
		bmc.End(nil)
	}
	power.End(nil)
	f.draw(time.Second)
	for _, node := range nodes[1:] {
		node.Run()
		node.End(nil)
	}
	ssh.End(nil)
	check(t, f.frames(),
		"read the power state · 0/3 · 1 running · 2 queued | run · 0/3 · 2 running · 1 queued · 0:02",
		"read the power state · 1/3 · 1 running · 1 queued | run · 1/3 · 1 failed · 1 running · 1 queued · 0:03",
		"run · 1/3 · 1 failed · 1 running · 1 queued · 0:04",
	)
}

// A step that counts nothing is named while it runs, unless it is hidden.
func TestTheCounterNamesTheStepUnderWay(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "provision reinstall", nil)
	_, hidden := progress.Start(f.ctx, progress.KindStep, "check the boot paths", progress.WithFlags(progress.Hidden))
	f.draw(time.Second)
	hidden.End(nil)
	_, step := progress.Start(f.ctx, progress.KindStep, "configuring the network boot")
	f.draw(time.Second)
	step.End(nil)
	check(t, f.frames(), "provision reinstall · 0:01", "configuring the network boot · 0:02")
}

// Whatever the command writes takes the line off first, and it is drawn
// again only once what was written ended a line: a question waiting for its
// answer is never drawn over.
func TestWritesTakeTheCounterOff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "bmc power cycle", nil)
	errOut := f.term.Writer(f.screen)
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "waiting 30s before the next batch\n")
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "About to power cycle 2 hosts\n")
	_, _ = io.WriteString(errOut, "Continue? [y/N] ")
	f.draw(time.Second)
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "y\n")
	f.draw(time.Second)
	want := `
<erase>bmc power cycle · 0:01
<erase>waiting 30s before the next batch

<erase>bmc power cycle · 0:02
<erase>About to power cycle 2 hosts
Continue? [y/N] y

<erase>bmc power cycle · 0:05`
	if got := f.screen.String(); got != want {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want)
	}
}

// Suspend takes the line off before it returns, and the line stays off,
// whatever is written meanwhile, until the question has been answered.
func TestSuspendTakesTheCounterOffUntilResumed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "bmc status", nil)
	errOut := f.term.Writer(f.screen)
	f.draw(time.Second)
	resume := progress.Suspend(f.ctx)
	if got := f.screen.String(); !strings.HasSuffix(got, "0:01\n<erase>") {
		t.Fatalf("Suspend returned with the line still drawn: %q", got)
	}
	_, _ = io.WriteString(errOut, "Password for admin@bmc: ")
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "\n")
	f.draw(time.Second)
	resume()
	f.draw(time.Second)
	want := `
<erase>bmc status · 0:01
<erase>Password for admin@bmc: 

<erase>bmc status · 0:04`
	if got := f.screen.String(); got != want {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want)
	}
}

// The line never wraps, which would leave a row behind each time it is
// drawn: it is cut to the width of the terminal, or to 80 columns when the
// terminal does not say.
func TestTheLineFitsTheTerminal(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 100)
	for _, tc := range []struct {
		name, command string
		size          func() (int, int, error)
		want          string
	}{
		{"a narrow terminal", "provision reinstall", func() (int, int, error) { return 21, 24, nil }, "provision reinstall "},
		{"a terminal that does not say", long, func() (int, int, error) { return 0, 0, errors.New("not a terminal") }, long[:79]},
		{"no size at all", long, nil, long[:79]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.command, tc.size)
			f.draw(time.Second)
			check(t, f.frames(), tc.want)
		})
	}
}

// Close takes the line off for good, stops the drawing Start began, and
// leaves the writers working.
func TestCloseTakesTheCounterOff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "exec", nil)
	errOut := f.term.Writer(f.screen)
	f.counter.Start()
	f.draw(time.Second)
	f.counter.Close()
	f.counter.Close()
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "clusterctl: interrupted\n")
	if got, want := f.screen.String(), "\n<erase>exec · 0:01\n<erase>clusterctl: interrupted\n"; got != want {
		t.Errorf("screen:\n%q\nwant:\n%q", got, want)
	}
}

// A display that panics while it draws, on its own goroutine, draws no
// more and says why with the stack, but the process goes on, and Close
// returns, as a Bus goes on without a sink that panics.
func TestADisplayThatPanicsWhileItDrawsStops(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"counter", "tree"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var log screen
			term := display.NewTerminal(&screen{}, func() (int, int, error) { panic("the size of the terminal") })
			term.PanicLog = &log
			c := &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
			var d interface {
				Start()
				Close()
			} = display.NewCounter(term, display.CounterOptions{Now: c.Now})
			if name == "tree" {
				d = display.NewTree(term, display.TreeOptions{Now: c.Now})
			}
			c.Add(2 * time.Second)
			d.Start()
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(log.String(), "the progress display stopped: the size of the terminal") {
				if time.Now().After(deadline) {
					t.Fatalf("no word of the panic within 5s: %q", log.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			d.Close()
			if !strings.Contains(log.String(), "goroutine") {
				t.Errorf("the stack is not written: %q", log.String())
			}
		})
	}
}

// A job in the background of its terminal draws nothing, since the shell's
// prompt is on the row a frame would erase, and draws again once it is in
// the foreground, below whatever the shell wrote meanwhile.
func TestNothingIsDrawnInTheBackground(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	inFront := false
	s := &screen{}
	term := display.NewTerminal(s, nil)
	term.Foreground = func() bool {
		mu.Lock()
		defer mu.Unlock()
		return inFront
	}
	c := &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	counter := display.NewCounter(term, display.CounterOptions{Now: c.Now})
	c.Add(2 * time.Second)
	counter.Draw()
	if got := s.String(); got != "" {
		t.Errorf("drawn in the background: %q", got)
	}
	mu.Lock()
	inFront = true
	mu.Unlock()
	counter.Draw()
	if got := s.String(); got == "" {
		t.Error("nothing drawn in the foreground")
	}
	counter.Close()
}
