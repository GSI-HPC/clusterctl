// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// treeFixture is a tree on a screen that shows what a terminal would, with
// the summary a display leaves behind, fed by a Bus on the same clock whose
// events are checked when the test ends, for a command that started when
// the tree was made. The terminal can be resized between two frames.
type treeFixture struct {
	ctx     context.Context
	screen  *progresstest.Screen
	term    *display.Terminal
	tree    *display.Tree
	summary *display.Summary
	clock   *clock
	bus     *progress.Bus
	command *progress.Span
	capture *progresstest.Capture

	mu            sync.Mutex
	width, height int
}

// treeSetup is the terminal a tree is drawn on: 100 columns and 24 rows
// unless it says otherwise.
type treeSetup struct {
	width, height int
	ascii         bool
	interrupted   <-chan struct{}
}

func newTreeFixture(t *testing.T, command string, o treeSetup) *treeFixture {
	t.Helper()
	f := &treeFixture{
		clock:  &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)},
		width:  cmp.Or(o.width, 100),
		height: cmp.Or(o.height, 24),
	}
	// A row drawn wider than the terminal wraps on the screen, as it
	// would on a terminal, so a test sees it.
	f.screen = &progresstest.Screen{Width: f.width}
	f.term = display.NewTerminal(f.screen, f.size)
	f.tree = display.NewTree(f.term, display.TreeOptions{Now: f.clock.Now, ASCII: o.ascii, Interrupted: o.interrupted})
	f.summary = &display.Summary{}
	f.capture = &progresstest.Capture{}
	f.bus = progress.NewBus(progress.Options{Sinks: []progress.Sink{f.capture, f.tree, f.summary}, Now: f.clock.Now})
	t.Cleanup(func() {
		f.close()
		progresstest.Check(t, f.capture.Events())
	})
	f.ctx, f.command = progress.Start(progress.WithBus(context.Background(), f.bus), progress.KindCommand, command)
	return f
}

func (f *treeFixture) size() (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.width, f.height, nil
}

func (f *treeFixture) resize(width, height int) {
	f.mu.Lock()
	f.width, f.height = width, height
	f.mu.Unlock()
}

// draw moves the clock on by d, draws a frame and returns what the screen
// shows.
func (f *treeFixture) draw(d time.Duration) string {
	f.clock.Add(d)
	f.tree.Draw()
	return f.screen.String()
}

// end ends the command with err, closes the Bus and the tree, and returns
// what the screen shows, the summary last, as the command line prints it.
func (f *treeFixture) end(err error) string {
	f.command.End(err)
	f.close()
	if line := f.summary.Line(); line != "" {
		_, _ = io.WriteString(f.screen, line+"\n")
	}
	return f.screen.String()
}

func (f *treeFixture) close() {
	f.bus.Close()
	f.tree.Close()
}

// targets starts a queued target for each of names under ctx, on its
// service processor, and returns them with their contexts.
func targets(ctx context.Context, names ...string) ([]context.Context, []*progress.Span) {
	ctxs, spans := make([]context.Context, len(names)), make([]*progress.Span, len(names))
	for i, node := range names {
		ctxs[i], spans[i] = progress.Start(ctx, progress.KindTarget, node, progress.Queued(),
			progress.Node(node), progress.Host(node+".mgmt"))
	}
	return ctxs, spans
}

func nodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("exe%d", i+1)
	}
	return out
}

// A command done within a second never shows the tree, nor the lines of
// the steps that finished meanwhile; once it has run for a second, the
// tree is drawn below them.
func TestTheTreeWaitsASecond(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "provision reinstall", treeSetup{})
	_, step := progress.Start(f.ctx, progress.KindStep, "configuring the network boot")
	if got := f.draw(500 * time.Millisecond); got != "" {
		t.Fatalf("the tree was drawn within its first second:\n%s", got)
	}
	step.End(nil)
	checkScreen(t, f.draw(500*time.Millisecond), `
✓ configuring the network boot  0.5s
provision reinstall · 0:01
`)

	quick := newTreeFixture(t, "provision reinstall", treeSetup{})
	_, step = progress.Start(quick.ctx, progress.KindStep, "configuring the network boot")
	quick.draw(500 * time.Millisecond)
	step.End(nil)
	quick.draw(400 * time.Millisecond)
	if got := quick.end(nil); got != "" {
		t.Errorf("a command done within a second left:\n%s", got)
	}
}

// A wide fan-out: the step says how its targets stand; the one that failed
// has a row, the running ones are listed the longest running first, with
// what each waits for, as many as fit, and the rest are counted; those that
// ended well are one node set, and those queued only a count. Once the
// step is over it leaves a line with how its targets ended, and the
// failures under it.
func TestTheTreeOfAFanOutWithManyQueued(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "provision reinstall", treeSetup{})
	ctx, step := progress.Start(f.ctx, progress.KindStep, "setting the machines to boot from the network once",
		progress.WithFlags(progress.Fold), progress.Total(480), progress.Limit(8))
	ctxs, spans := targets(ctx, nodes(480)...)
	end := func(i int) {
		var err error
		if i == 40 {
			err = exitcode.Errorf(exitcode.Transport, "exe41.mgmt: dial tcp: i/o timeout")
		}
		spans[i].End(err)
	}
	for i := range 312 {
		spans[i].Run()
		end(i)
	}
	for i := 312; i < 320; i++ {
		f.clock.Add(500 * time.Millisecond)
		spans[i].Run()
	}
	progress.Start(ctxs[312], progress.KindCall, "redfish", progress.HTTP("PATCH", "/redfish/v1/Systems/1"), progress.Host("exe313.mgmt"))
	progress.Start(ctxs[314], progress.KindCall, "ssh", progress.Node("exe315"), progress.Timeout(10*time.Minute))
	checkScreen(t, f.draw(4*time.Minute), `
provision reinstall · 4:04
  setting the machines to boot from the network once  312/480 · 1 failed · 8 running · 160 queued
    ✗ exe41  transport: {}: dial tcp: i/o timeout
    ▸ exe313  4m03s  PATCH /redfish/v1/Systems/1
    ▸ exe314  4m03s
    ▸ exe315  4m00s/10m  ssh
    … 5 more running
    ✓ exe[1-40,42-312]
`)
	for i := 312; i < 480; i++ {
		spans[i].Run()
		end(i)
	}
	step.End(errors.New("1 of 480 failed: exe41"))
	checkScreen(t, f.draw(time.Second), `
✗ setting the machines to boot from the network once  4m04s  479 ok, 1 failed
  ✗ exe41  transport: {}: dial tcp: i/o timeout
provision reinstall · 4:05
`)
}

// Targets that fail alike share a row, with their own names read as {}:
// the class and the error are what they share, not the host they name.
func TestTheTreeGroupsFailures(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "bmc power off", treeSetup{})
	ctx, step := progress.Start(f.ctx, progress.KindStep, "power off",
		progress.WithFlags(progress.Fold), progress.Total(7), progress.Limit(8))
	_, spans := targets(ctx, nodes(7)...)
	for _, span := range spans {
		span.Run()
	}
	for i, span := range spans[:6] {
		node := fmt.Sprintf("exe%d", i+1)
		switch i % 3 {
		case 0:
			span.End(exitcode.Errorf(exitcode.Transport, "%s.mgmt: dial tcp: i/o timeout", node))
		case 1:
			span.End(fmt.Errorf("%s.mgmt: 400 Bad Request: refused", node))
		default:
			span.End(nil)
		}
	}
	checkScreen(t, f.draw(2*time.Second), `
bmc power off · 0:02
  power off  6/7 · 4 failed · 1 running
    ✗ exe[1,4]  transport: {}: dial tcp: i/o timeout
    ✗ exe[2,5]  target: {}: 400 Bad Request: refused
    ▸ exe7  2s
    ✓ exe[3,6]
`)
	spans[6].End(nil)
	step.End(errors.New("4 of 7 failed: exe[1-2,4-5]"))
	checkScreen(t, f.end(errors.New("4 of 7 failed: exe[1-2,4-5]")), `
✗ power off  2.0s  3 ok, 4 failed
  ✗ exe[1,4]  transport: {}: dial tcp: i/o timeout
  ✗ exe[2,5]  target: {}: 400 Bad Request: refused
bmc power off: failed in 2.0s: 3 ok, 4 failed
`)
}

// Processors that refuse the connection each name their own address, as
// a dialer does: they still fail alike.
func TestTheTreeGroupsFailuresThatNameTheirAddresses(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "bmc power off", treeSetup{})
	ctx, step := progress.Start(f.ctx, progress.KindStep, "power off",
		progress.WithFlags(progress.Fold), progress.Total(3), progress.Limit(8))
	_, spans := targets(ctx, nodes(3)...)
	for _, span := range spans {
		span.Run()
	}
	f.draw(2 * time.Second)
	for i, span := range spans {
		span.End(exitcode.Errorf(exitcode.Transport,
			"Post \"https://exe%d.mgmt/redfish/v1\": dial tcp 10.0.0.%d:443: connect: connection refused", i+1, i+7))
	}
	step.End(errors.New("3 of 3 failed: exe[1-3]"))
	checkScreen(t, f.end(errors.New("3 of 3 failed: exe[1-3]")), `
✗ power off  2.0s  0 ok, 3 failed
  ✗ exe[1-3]  transport: Post "https://{}/redfish/v1": dial tcp {}: connect: connection refused
bmc power off: failed in 2.0s: 0 ok, 3 failed
`)
}

// Plumbing is hidden: a lookup is drawn only once it has taken a second,
// and leaves a line only if it took that long or failed.
func TestTheTreeRevealsASlowHiddenSpan(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "provision reinstall", treeSetup{})
	_, quick := progress.Start(f.ctx, progress.KindStep, "resolve", progress.WithFlags(progress.Hidden))
	f.clock.Add(40 * time.Millisecond)
	quick.End(nil)
	f.clock.Add(460 * time.Millisecond)
	ctx, slow := progress.Start(f.ctx, progress.KindStep, "check the boot paths", progress.WithFlags(progress.Hidden))
	_, call := progress.Start(ctx, progress.KindCall, "ssh", progress.Node("install"), progress.Timeout(10*time.Minute))
	checkScreen(t, f.draw(500*time.Millisecond), `
provision reinstall · 0:01
`)
	checkScreen(t, f.draw(time.Second), `
provision reinstall · 0:02
  check the boot paths  1s/10m  ssh install
`)
	call.End(nil)
	slow.End(nil)
	_, failed := progress.Start(f.ctx, progress.KindStep, "check Slurm jobs", progress.WithFlags(progress.Hidden))
	f.clock.Add(10 * time.Millisecond)
	failed.End(exitcode.Errorf(exitcode.Transport, "login: connection refused"))
	checkScreen(t, f.draw(time.Second), `
✓ check the boot paths  1.5s
✗ check Slurm jobs  0.0s
provision reinstall · 0:03
`)
}

// A step that ends well at once, with nothing drawn below it, leaves no
// line; one that failed as quickly keeps its line, and so does one that
// had something drawn below it.
func TestAFastFailureKeepsItsLine(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "provision reinstall", treeSetup{})
	f.draw(time.Second)
	quick := func(name string, err error, below bool) {
		ctx, step := progress.Start(f.ctx, progress.KindStep, name)
		if below {
			fanout.Map(ctx, []string{"exe1"}, fanout.Options[string]{Step: "clearing the boot overrides"},
				func(context.Context, string) (struct{}, error) { return struct{}{}, nil })
		}
		f.clock.Add(40 * time.Millisecond)
		step.End(err)
	}
	quick("forgetting the host keys", nil, false)
	quick("configuring the network boot", exitcode.Errorf(exitcode.Transport, "install: connection refused"), false)
	quick("disarming", nil, true)
	checkScreen(t, f.draw(time.Second), `
✗ configuring the network boot  0.0s
✓ disarming › clearing the boot overrides  0.0s  1 ok
✓ disarming  0.0s
provision reinstall · 0:02
`)
}

// A power-on in batches: the batch under way has a row under the step,
// with its targets; the pause between two counts down; what a batch that
// is over did is folded into the step, a failure too, and so are the
// batches left out after it.
func TestTheTreeOfAPowerOnInBatches(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "bmc power on", treeSetup{})
	set, err := nodeset.Parse("exe[1-6]")
	if err != nil {
		t.Fatal(err)
	}
	var frames []string
	f.clock.Add(time.Second)
	fanout.Batches(f.ctx, set, fanout.BatchOptions{
		Step:  "power on",
		Size:  2,
		Pause: 30 * time.Second,
		After: func(d time.Duration) <-chan time.Time {
			frames = append(frames, f.draw(10*time.Second))
			f.clock.Add(d - 10*time.Second)
			ready := make(chan time.Time, 1)
			ready <- f.clock.Now()
			return ready
		},
	}, func(ctx context.Context, batch *nodeset.NodeSet) error {
		_, spans := targets(ctx, batch.Expand()...)
		var failed error
		for i, node := range batch.Expand() {
			spans[i].Run()
			frames = append(frames, f.draw(time.Second))
			var err error
			if node == "exe3" {
				err = errors.New("exe3.mgmt: connection timeout")
				failed = errors.New("1 of 2 service processors failed")
			}
			spans[i].End(err)
		}
		return failed
	})
	checkScreen(t, frames[0], `
bmc power on · 0:02
  power on  0/6 · 1 running · 5 queued
    batch 1/3  0/2 · 1 running · 1 queued
      ▸ exe1  1s
`)
	checkScreen(t, frames[2], `
bmc power on · 0:13
  power on  2/6 · 4 queued
    stagger  20s left
    ✓ exe[1-2]
`)
	checkScreen(t, frames[4], `
bmc power on · 0:35
  power on  3/6 · 1 failed · 1 running · 2 queued
    batch 2/3  1/2 · 1 failed · 1 running
      ✗ exe3  target: {}: connection timeout
      ▸ exe4  1s
    ✓ exe[1-2]
`)
	checkScreen(t, f.end(errors.New("1 of 2 service processors failed")), `
✗ power on  34s  3 ok, 1 failed, 2 skipped
  ✗ exe3  target: {}: connection timeout
bmc power on: failed in 35s: 3 ok, 1 failed, 2 skipped
`)
}

// Two steps under way side by side share the rows there are: each lists
// what runs, as far as it gets its turn, and counts the rest.
func TestTheTreeShowsTwoRootsSideBySide(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "provision status", treeSetup{})
	start := func(name string, call func(ctx context.Context)) {
		ctx, _ := progress.Start(f.ctx, progress.KindStep, name, progress.WithFlags(progress.Fold), progress.Total(5))
		ctxs, spans := targets(ctx, nodes(5)...)
		for i, span := range spans {
			f.clock.Add(100 * time.Millisecond)
			span.Run()
			call(ctxs[i])
		}
	}
	start("read the power state", func(ctx context.Context) {
		progress.Start(ctx, progress.KindCall, "redfish", progress.HTTP("GET", "/redfish/v1/Systems/1"))
	})
	start("read the uptime", func(ctx context.Context) {
		progress.Start(ctx, progress.KindCall, "ssh", progress.Timeout(20*time.Second))
	})
	checkScreen(t, f.draw(time.Second), `
provision status · 0:02
  read the power state  0/5 · 5 running
    ▸ exe1  1s  GET /redfish/v1/Systems/1
    ▸ exe2  1s  GET /redfish/v1/Systems/1
    … 3 more running
  read the uptime  0/5 · 5 running
    ▸ exe1  1s/20s  ssh
    … 4 more running
`)
}

// On a terminal too small for the tree the counter's line is drawn
// instead, as the terminal's size reads at each frame.
func TestTheTreeFallsBackToTheCounter(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "exec", treeSetup{width: 100, height: 7})
	ctx, _ := progress.Start(f.ctx, progress.KindStep, "run", progress.WithFlags(progress.Fold), progress.Total(3))
	_, spans := targets(ctx, nodes(3)...)
	spans[0].Run()
	checkScreen(t, f.draw(time.Second), `
run · 0/3 · 1 running · 2 queued · 0:01
`)
	f.resize(100, 8)
	checkScreen(t, f.draw(time.Second), `
exec · 0:02
  run  0/3 · 1 running · 2 queued
    ▸ exe1  2s
`)
	f.resize(39, 40)
	checkScreen(t, f.draw(time.Second), `
run · 0/3 · 1 running · 2 queued · 0:0
`)
}

// Outside a UTF-8 locale the tree is drawn in ASCII alone, and so is the
// counter in its place.
func TestTheTreeInASCII(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "exec", treeSetup{ascii: true})
	ctx, step := progress.Start(f.ctx, progress.KindStep, "run", progress.WithFlags(progress.Fold), progress.Total(5), progress.Limit(1))
	_, spans := targets(ctx, nodes(5)...)
	for i, span := range spans[:4] {
		span.Run()
		switch i {
		case 1:
			span.End(errors.New("exe2: command exited 1"))
		case 2:
			span.End(context.Canceled)
		case 3:
			span.Skip("the node could not be reached")
		default:
			span.End(nil)
		}
	}
	spans[4].Run()
	tree := f.draw(time.Second)
	checkScreen(t, tree, `
exec - 0:01
  run  4/5 - 1 failed - 1 canceled - 1 skipped - 1 running
    x exe2  target: {}: command exited 1
    ~ exe3  canceled
    - exe4  skipped: the node could not be reached
    > exe5  1s
    + exe1
`)
	f.resize(30, 24)
	counter := f.draw(time.Second)
	checkScreen(t, counter, `
run - 4/5 - 1 failed - 1 canc
`)
	spans[4].End(nil)
	step.End(errors.New("1 of 5 failed: exe2"))
	end := f.end(errors.New("1 of 5 failed: exe2"))
	checkScreen(t, end, `
x run  2.0s  2 ok, 1 failed, 1 canceled, 1 skipped
  x exe2  target: {}: command exited 1
exec: failed in 2.0s: 2 ok, 1 failed, 1 canceled, 1 skipped
`)
	for _, frame := range []string{tree, counter, end} {
		for _, r := range frame {
			if r > 0x7e {
				t.Fatalf("the ASCII tree drew %q:\n%s", r, frame)
			}
		}
	}
}

// Suspend takes the region off before it returns, and leaves only what the
// command printed, so that a question is asked below it; the region comes
// back once the question has been answered.
func TestSuspendLeavesTheRegionEmpty(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "slurm node drain", treeSetup{})
	errOut := f.term.Writer(f.screen)
	_, step := progress.Start(f.ctx, progress.KindStep, "check Slurm jobs")
	_, _ = io.WriteString(errOut, "exe7 runs no jobs\n")
	f.draw(time.Second)
	step.End(nil)
	resume := progress.Suspend(f.ctx)
	checkScreen(t, f.screen.String(), `
exe7 runs no jobs
✓ check Slurm jobs  1.0s
`)
	_, _ = io.WriteString(errOut, "About to drain 1 host: exe7\nContinue? [y/N] ")
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "y\n")
	f.draw(time.Second)
	resume()
	checkScreen(t, f.draw(time.Second), `
exe7 runs no jobs
✓ check Slurm jobs  1.0s
About to drain 1 host: exe7
Continue? [y/N] y
slurm node drain · 0:04
`)
}

// A step that finished in the first second, before the command wrote
// something, leaves no line: written at the first frame, it would stand
// below what came after it.
func TestAStepThatEndedBeforeAWriteInTheFirstSecondLeavesNoLine(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "slurm node drain", treeSetup{})
	errOut := f.term.Writer(f.screen)
	_, read := progress.Start(f.ctx, progress.KindStep, "read the inventory")
	f.clock.Add(300 * time.Millisecond)
	read.End(nil)
	_, _ = io.WriteString(errOut, "exe7 runs no jobs\n")
	_, step := progress.Start(f.ctx, progress.KindStep, "check Slurm jobs")
	f.draw(time.Second)
	step.End(nil)
	checkScreen(t, f.draw(100*time.Millisecond), `
exe7 runs no jobs
✓ check Slurm jobs  1.0s
slurm node drain · 0:01
`)
}

// What the command writes while the tree is drawn takes the region off
// first, and the region is drawn again below it.
func TestAWriteLandsAboveTheRegion(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "bmc power cycle", treeSetup{})
	errOut := f.term.Writer(f.screen)
	ctx, _ := progress.Start(f.ctx, progress.KindStep, "power cycle", progress.WithFlags(progress.Fold), progress.Total(2))
	_, spans := targets(ctx, "exe1", "exe2")
	spans[0].Run()
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "powering on exe[1-2] (1 of 1)\n")
	checkScreen(t, f.screen.String(), `
powering on exe[1-2] (1 of 1)
`)
	checkScreen(t, f.draw(time.Second), `
powering on exe[1-2] (1 of 1)
bmc power cycle · 0:02
  power cycle  0/2 · 1 running · 1 queued
    ▸ exe1  2s
`)
	// A line the command has not ended yet is not drawn over.
	_, _ = io.WriteString(errOut, "Password: ")
	checkScreen(t, f.draw(time.Second), `
powering on exe[1-2] (1 of 1)
Password: 
`)
}

// Once the command has been interrupted, its row says so, and how many
// targets stop and how many will not start.
func TestTheTreeSaysItIsInterrupting(t *testing.T) {
	t.Parallel()
	interrupt := make(chan struct{})
	f := newTreeFixture(t, "exec", treeSetup{interrupted: interrupt})
	ctx, _ := progress.Start(f.ctx, progress.KindStep, "run", progress.WithFlags(progress.Fold), progress.Total(5), progress.Limit(2))
	_, spans := targets(ctx, nodes(5)...)
	spans[0].Run()
	spans[1].Run()
	f.draw(time.Second)
	close(interrupt)
	checkScreen(t, f.draw(time.Second), `
exec · interrupting · 2 running will stop · 3 queued will not start · 0:02
  run  0/5 · 2 running · 3 queued
    ▸ exe1  2s
    ▸ exe2  2s
`)
	for _, span := range spans {
		span.End(context.Canceled)
	}
	checkScreen(t, f.draw(time.Second), `
exec · interrupting · 0:03
  run  5/5 · 5 canceled
    ⊘ exe[1-5]  canceled
`)
}

// A step that shows lines has each running target show the last line of
// its output, in place of the request it waits for.
func TestTheTreeShowsTheLastLineOfOutput(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "cinc run", treeSetup{})
	ctx, _ := progress.Start(f.ctx, progress.KindStep, "run",
		progress.WithFlags(progress.Fold|progress.ShowLines), progress.Total(2))
	ctxs, spans := targets(ctx, "exe1", "exe2")
	for i, span := range spans {
		span.Run()
		callCtx, _ := progress.Start(ctxs[i], progress.KindCall, "ssh", progress.Timeout(30*time.Minute))
		if i == 0 {
			out := progress.Tee(callCtx, io.Discard, progress.Stdout, nil)
			_, _ = io.WriteString(out, "Starting Cinc Client\nConverging 12 resources\n")
		}
	}
	checkScreen(t, f.draw(90*time.Second), `
cinc run · 1:30
  run  0/2 · 2 running
    ▸ exe1  1m30s/30m  Converging 12 resources
    ▸ exe2  1m30s/30m  ssh
`)
}

// No row is drawn wider than the terminal, which would wrap it and leave a
// row behind at the next frame: each is cut to the width.
func TestTheRowsFitTheTerminal(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "exec", treeSetup{width: 40})
	ctx, _ := progress.Start(f.ctx, progress.KindStep, "run the command on every node of the cluster",
		progress.WithFlags(progress.Fold), progress.Total(1))
	_, spans := targets(ctx, "exe1")
	spans[0].Run()
	spans[0].End(errors.New("exe1: " + strings.Repeat("ü", 60)))
	f.draw(time.Second)
	checkScreen(t, f.draw(time.Second), `
exec · 0:02
  run the command on every node of the 
    ✗ exe1  target: {}: üüüüüüüüüüüüüüü
`)
}

// A row of wide characters, as CJK text is, is cut by the columns it takes,
// two a character, so that it does not wrap on a terminal of that width.
func TestARowOfWideCharactersFitsTheTerminal(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "exec", treeSetup{width: 40})
	ctx, _ := progress.Start(f.ctx, progress.KindStep, "run", progress.WithFlags(progress.Fold), progress.Total(1))
	_, spans := targets(ctx, "exe1")
	spans[0].Run()
	spans[0].End(errors.New("exe1: " + strings.Repeat("失败", 40)))
	f.draw(time.Second)
	checkScreen(t, f.draw(time.Second), `
exec · 0:02
  run  1/1 · 1 failed
    ✗ exe1  target: {}: 失败失败失败失
`)
}

// A step that is part of a target's work is drawn in the target's row, and
// leaves no line of its own once it is over: the target's fold says how it
// ended.
func TestAStepOfATargetIsPartOfItsRow(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "secrets push", treeSetup{})
	fanout.Map(f.ctx, []string{"exe1", "exe2"}, fanout.Options[string]{Step: "write the secrets", Limit: 1},
		func(ctx context.Context, node string) (struct{}, error) {
			for _, file := range []string{"/etc/munge/munge.key", "/etc/ssh/ssh_host_ed25519_key"} {
				ctx, step := progress.Start(ctx, progress.KindStep, "write "+file)
				_, call := progress.Start(ctx, progress.KindCall, "ssh", progress.Node(node), progress.Timeout(30*time.Second))
				if node == "exe2" && file == "/etc/munge/munge.key" {
					checkScreen(t, f.draw(time.Second), `
secrets push · 0:03
  write the secrets  1/2 · 1 running
    ▸ exe2  1s/30s  ssh
    ✓ exe1
`)
				}
				f.clock.Add(time.Second)
				call.End(nil)
				step.End(nil)
			}
			return struct{}{}, nil
		})
	checkScreen(t, f.draw(time.Second), `
✓ write the secrets  5.0s  2 ok
secrets push · 0:06
`)
}

// A step with no name is no row: what is below it is drawn in its place.
func TestAStepWithNoNamePassesItsChildrenUp(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "node hw", treeSetup{})
	ctx, unnamed := progress.Start(f.ctx, progress.KindStep, "")
	_, step := progress.Start(ctx, progress.KindStep, "read the inventory")
	askCtx, ask := progress.Start(ctx, progress.KindStep, "ask the nodes", progress.WithFlags(progress.Fold), progress.Total(1))
	_, spans := targets(askCtx, "exe1")
	spans[0].Run()
	checkScreen(t, f.draw(time.Second), `
node hw · 0:01
  read the inventory  1s
  ask the nodes  0/1 · 1 running
    ▸ exe1  1s
`)
	step.End(nil)
	spans[0].End(nil)
	ask.End(nil)
	unnamed.End(nil)
	checkScreen(t, f.draw(time.Second), `
✓ read the inventory  1.0s
✓ ask the nodes  1.0s  1 ok
node hw · 0:02
`)
}

// Close takes the region off for good, writes the lines left, stops the
// drawing Start began, and leaves the writers working.
func TestCloseTakesTheTreeOff(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t, "exec", treeSetup{})
	errOut := f.term.Writer(f.screen)
	f.tree.Start()
	_, step := progress.Start(f.ctx, progress.KindStep, "run")
	f.draw(time.Second)
	step.End(nil)
	f.tree.Close()
	f.tree.Close()
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "clusterctl: interrupted\n")
	checkScreen(t, f.screen.String(), `
✓ run  1.0s
clusterctl: interrupted
`)
}
