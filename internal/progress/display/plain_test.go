// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package display_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/display"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// plainFixture is a display of plain lines on a screen, with the summary a
// display leaves behind, fed by a Bus on the same clock whose events are
// checked when the test ends, for a command that started when the display
// was made.
type plainFixture struct {
	ctx     context.Context
	screen  *screen
	term    *display.Terminal
	plain   *display.Plain
	summary *display.Summary
	clock   *clock
	bus     *progress.Bus
	command *progress.Span
	capture *progresstest.Capture
}

func newPlainFixture(t *testing.T, command string) *plainFixture {
	t.Helper()
	f := &plainFixture{screen: &screen{}, clock: &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}}
	f.term = display.NewTerminal(f.screen, nil)
	f.plain = display.NewPlain(f.term, display.PlainOptions{Now: f.clock.Now})
	f.summary = &display.Summary{}
	f.capture = &progresstest.Capture{}
	f.bus = progress.NewBus(progress.Options{Sinks: []progress.Sink{f.capture, f.plain, f.summary}, Now: f.clock.Now})
	t.Cleanup(func() {
		f.close()
		progresstest.Check(t, f.capture.Events())
	})
	f.ctx, f.command = progress.Start(progress.WithBus(context.Background(), f.bus), progress.KindCommand, command)
	return f
}

// draw moves the clock on by d and writes what is due.
func (f *plainFixture) draw(d time.Duration) {
	f.clock.Add(d)
	f.plain.Draw()
}

// end ends the command with err, closes the Bus and the display, and
// returns what the screen shows, the summary last, as the command line
// prints it.
func (f *plainFixture) end(err error) string {
	f.command.End(err)
	f.close()
	if line := f.summary.Line(); line != "" {
		_, _ = io.WriteString(f.screen, line+"\n")
	}
	return f.screen.String()
}

func (f *plainFixture) close() {
	f.bus.Close()
	f.plain.Close()
}

func checkScreen(t *testing.T, got, want string) {
	t.Helper()
	if got != want[1:] {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want[1:])
	}
}

// A fan-out: the step's start with its size, each target that fails once
// with why, where the step stands every ten seconds, and its end with the
// count of each outcome; the summary after it counts the nodes once.
func TestPlainLinesOfAFanOut(t *testing.T) {
	t.Parallel()
	f := newPlainFixture(t, "bmc power off")
	nodes := []string{"exe1", "exe2", "exe3", "exe4", "exe5", "exe6"}
	outcomes := fanout.Map(f.ctx, nodes, fanout.Options[string]{Step: "power off", Limit: 1, Describe: onBMC},
		func(_ context.Context, node string) (struct{}, error) {
			f.draw(3 * time.Second)
			if node == "exe3" || node == "exe5" {
				return struct{}{}, exitcode.Errorf(exitcode.Transport, "%s.mgmt: dial tcp: i/o timeout", node)
			}
			return struct{}{}, nil
		})
	if len(outcomes) != len(nodes) {
		t.Fatalf("%d outcomes, want %d", len(outcomes), len(nodes))
	}
	checkScreen(t, f.end(errors.New("2 of 6 hosts failed: exe[3,5]")), `
[0:00] bmc power off › power off: start, 6 hosts, 1 at a time
[0:09] bmc power off › power off › exe3 failed (transport): exe3.mgmt: dial tcp: i/o timeout
[0:12] bmc power off › power off: 3/6 done, 1 failed, 1 running, 2 queued
[0:15] bmc power off › power off › exe5 failed (transport): exe5.mgmt: dial tcp: i/o timeout
[0:18] bmc power off › power off: failed in 18s: 4 ok, 2 failed
bmc power off: failed in 18s: 4 ok, 2 failed
`)
}

// onBMC describes a node by its service processor.
func onBMC(node string) (string, string, string) { return node, node + ".mgmt", "" }

// A power-on in batches: each batch starts and ends, with the pause before
// the next; the whole step says where it stands; a batch left out after
// one that failed says so and why.
func TestPlainLinesOfAPowerOnInBatches(t *testing.T) {
	t.Parallel()
	f := newPlainFixture(t, "bmc power on")
	nodes, err := nodeset.Parse("exe[1-6]")
	if err != nil {
		t.Fatal(err)
	}
	batches := fanout.Batches(f.ctx, nodes, fanout.BatchOptions{
		Step:  "power on",
		Size:  2,
		Pause: 5 * time.Second,
		After: func(d time.Duration) <-chan time.Time {
			f.draw(d)
			ready := make(chan time.Time, 1)
			ready <- f.clock.Now()
			return ready
		},
	}, func(ctx context.Context, batch *nodeset.NodeSet) error {
		names := batch.Expand()
		spans := make([]*progress.Span, len(names))
		for i, node := range names {
			_, spans[i] = progress.Start(ctx, progress.KindTarget, node, progress.Queued(), progress.Node(node))
		}
		var failed error
		for i, node := range names {
			spans[i].Run()
			f.draw(2 * time.Second)
			var err error
			if node == "exe4" {
				err = errors.New("exe4.mgmt: connection timeout")
				failed = errors.New("1 of 2 service processors failed")
			}
			spans[i].End(err)
		}
		return failed
	})
	if batches[2].Ran {
		t.Fatal("the batch after the one that failed was run")
	}
	checkScreen(t, f.end(batches[1].Err), `
[0:00] bmc power on › power on: start, 6 hosts
[0:00] bmc power on › power on › batch 1/3: start, 2 hosts
[0:04] bmc power on › power on › batch 1/3: done in 4.0s: 2 ok
[0:04] bmc power on › power on › stagger: waiting 5s
[0:09] bmc power on › power on › batch 2/3: start, 2 hosts
[0:11] bmc power on › power on: batch 2/3, 2/6 done, 1 running, 3 queued
[0:13] bmc power on › power on › batch 2/3 › exe4 failed (target): exe4.mgmt: connection timeout
[0:13] bmc power on › power on › batch 2/3: failed in 4.0s: 1 ok, 1 failed
[0:13] bmc power on › power on › batch 3/3: skipped: not tried: an earlier batch failed
[0:13] bmc power on › power on: failed in 13s: 3 ok, 1 failed, 2 skipped
bmc power on: failed in 13s: 3 ok, 1 failed, 2 skipped
`)
}

// A failure after several steps: the steps that count nothing say only how
// they ended and how long they took, hidden ones and calls nothing, and a
// node counted by two steps is one node in the summary, whose count is
// all the line says of the failure: the command's error says the rest.
func TestPlainLinesOfAFailure(t *testing.T) {
	t.Parallel()
	f := newPlainFixture(t, "provision reinstall")
	_, hidden := progress.Start(f.ctx, progress.KindStep, "resolve", progress.WithFlags(progress.Hidden))
	f.draw(time.Second)
	hidden.End(nil)
	boot, links := progress.Start(f.ctx, progress.KindStep, "configuring the network boot")
	_, call := progress.Start(boot, progress.KindCall, "ssh", progress.Node("install"))
	f.draw(2 * time.Second)
	call.End(nil)
	links.End(nil)
	nodes := []string{"exe1", "exe2", "exe3"}
	fanout.Map(f.ctx, nodes, fanout.Options[string]{Step: "setting the machines to boot from the network once", Limit: 8},
		func(_ context.Context, node string) (struct{}, error) {
			if node == "exe2" {
				f.draw(time.Second)
				return struct{}{}, exitcode.Errorf(exitcode.Transport, "exe2.mgmt: dial tcp: connection refused")
			}
			return struct{}{}, nil
		})
	disarm, disarming := progress.Start(f.ctx, progress.KindStep, "disarming")
	fanout.Map(disarm, []string{"exe1", "exe3"}, fanout.Options[string]{Step: "clearing the boot overrides", Limit: 8},
		func(context.Context, string) (struct{}, error) { return struct{}{}, nil })
	disarming.End(nil)
	checkScreen(t, f.end(exitcode.Errorf(exitcode.Transport, "setting the machines to boot from the network once failed: exe2")), `
[0:01] provision reinstall › configuring the network boot: start
[0:03] provision reinstall › configuring the network boot: done in 2.0s
[0:03] provision reinstall › setting the machines to boot from the network once: start, 3 hosts
[0:04] provision reinstall › setting the machines to boot from the network once › exe2 failed (transport): exe2.mgmt: dial tcp: connection refused
[0:04] provision reinstall › setting the machines to boot from the network once: failed in 1.0s: 2 ok, 1 failed
[0:04] provision reinstall › disarming: start
[0:04] provision reinstall › disarming › clearing the boot overrides: start, 2 hosts
[0:04] provision reinstall › disarming › clearing the boot overrides: done in 0.0s: 2 ok
[0:04] provision reinstall › disarming: done in 0.0s
provision reinstall: failed in 4.0s: 2 ok, 1 failed
`)
}

// Each counted step under way says where it stands every ten seconds, not
// at every look, and those side by side each for itself.
func TestPlainHeartbeatsEveryTenSeconds(t *testing.T) {
	t.Parallel()
	f := newPlainFixture(t, "provision status")
	start := func(name string) (*progress.Span, *progress.Span) {
		ctx, step := progress.Start(f.ctx, progress.KindStep, name, progress.WithFlags(progress.Fold), progress.Total(1))
		_, target := progress.Start(ctx, progress.KindTarget, "exe1", progress.Queued(), progress.Node("exe1"))
		target.Run()
		return step, target
	}
	power, bmc := start("read the power state")
	f.draw(4 * time.Second)
	ssh, node := start("read the uptime")
	for range 12 {
		f.draw(time.Second)
	}
	bmc.End(nil)
	power.End(nil)
	f.draw(10 * time.Second)
	node.End(nil)
	ssh.End(nil)
	checkScreen(t, f.end(nil), `
[0:00] provision status › read the power state: start, 1 host
[0:04] provision status › read the uptime: start, 1 host
[0:10] provision status › read the power state: 0/1 done, 1 running
[0:14] provision status › read the uptime: 0/1 done, 1 running
[0:16] provision status › read the power state: done in 16s: 1 ok
[0:26] provision status › read the uptime: 0/1 done, 1 running
[0:26] provision status › read the uptime: done in 22s: 1 ok
provision status: done in 26s: 1 ok
`)
}

// The lines go out ahead of whatever the command writes after the events
// they tell of, never into a line it has not ended, and not while a
// question is asked: those that came meanwhile follow once it is answered.
func TestPlainLinesKeepClearOfTheCommandsOwn(t *testing.T) {
	t.Parallel()
	f := newPlainFixture(t, "bmc power cycle")
	errOut := f.term.Writer(f.screen)
	_, step := progress.Start(f.ctx, progress.KindStep, "check Slurm jobs")
	_, _ = io.WriteString(errOut, "exe[1-2] run no jobs\n")
	_, _ = io.WriteString(errOut, "About to power cycle 2 hosts\n")
	resume := progress.Suspend(f.ctx)
	_, _ = io.WriteString(errOut, "Continue? [y/N] ")
	step.End(nil)
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "y\n")
	f.draw(time.Second)
	resume()
	_, _ = io.WriteString(errOut, "Password:")
	_, wait := progress.Start(f.ctx, progress.KindWait, "stagger", progress.Timeout(30*time.Second))
	f.draw(time.Second)
	_, _ = io.WriteString(errOut, "\n")
	wait.End(context.Canceled)
	checkScreen(t, f.end(context.Canceled), `
[0:00] bmc power cycle › check Slurm jobs: start
exe[1-2] run no jobs
About to power cycle 2 hosts
Continue? [y/N] y
[0:00] bmc power cycle › check Slurm jobs: done in 0.0s
Password:
[0:02] bmc power cycle › stagger: waiting 30s
[0:03] bmc power cycle › stagger: canceled
bmc power cycle: canceled in 3.0s
`)
}

// Nothing but plain text reaches the screen: what a host said is escaped
// before it is shown, and the lines carry no escape codes of their own.
func TestPlainLinesCarryNoEscapeCodes(t *testing.T) {
	t.Parallel()
	f := newPlainFixture(t, "exec")
	fanout.Map(f.ctx, []string{"exe1"}, fanout.Options[string]{Step: "run"},
		func(context.Context, string) (struct{}, error) {
			return struct{}{}, errors.New("exe1: \x1b[2Jcleared\x07")
		})
	got := f.end(nil)
	if strings.ContainsAny(got, "\x1b\x07\r") {
		t.Errorf("the screen got an escape code or a control character: %q", got)
	}
	if !strings.Contains(got, `exe1 failed (target): exe1: \x1b[2Jcleared\x07`) {
		t.Errorf("the failure is not shown escaped:\n%s", got)
	}
}

// Start writes a line as it comes, and a heartbeat as it falls due, without
// a call to Draw; Close stops it and writes what is left.
func TestPlainStartWritesTheLinesAsTheyCome(t *testing.T) {
	t.Parallel()
	f := newPlainFixture(t, "exec")
	f.plain.Start()
	_, step := progress.Start(f.ctx, progress.KindStep, "run")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.screen.String(), "run: start") {
		if time.Now().After(deadline) {
			t.Fatalf("the line of the step was not written: %q", f.screen.String())
		}
		time.Sleep(time.Millisecond)
	}
	step.End(nil)
	if got := f.end(nil); !strings.HasSuffix(got, "exec › run: done in 0.0s\n") {
		t.Errorf("Close did not write the last line:\n%s", got)
	}
}
