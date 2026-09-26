// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package safety_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/safety"
)

// terminal records, in order, what the gate prints and when a display
// takes itself off the terminal and puts itself back.
type terminal struct {
	mu  sync.Mutex
	log []string
}

func (t *terminal) Write(p []byte) (int, error) {
	t.add(strings.TrimRight(string(p), " "))
	return len(p), nil
}

func (t *terminal) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.log = append(t.log, s)
}

func (t *terminal) Handle(progress.Event) {}
func (t *terminal) Suspend()              { t.add("[suspend]") }
func (t *terminal) Resume()               { t.add("[resume]") }

func (t *terminal) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.log, "|")
}

// The gate is a wait that ends as the action may go ahead or not: well when
// it may, skipped in a dry run, failed when it is refused, and canceled
// when the answer is no or the command is interrupted. A display is off
// the terminal while the question is asked and answered, and back once it
// is, however that ends: after the newline an interrupt puts under the
// question too.
func TestTheGateIsReportedAsAWait(t *testing.T) {
	t.Parallel()
	const asked = "[suspend]|About to drain 2 hosts: exe[1-2]\n|Continue? [y/N]"
	for _, tc := range []struct {
		name   string
		setup  func(g *safety.Gate, cancel func())
		nodes  string
		answer string
		want   string
		shown  string
	}{
		{"accepted", nil, "exe[1-2]", "y\n",
			"wait confirm message=drain 2 hosts: ok\n", asked + "|[resume]"},
		{"declined", nil, "exe[1-2]", "n\n",
			"wait confirm message=drain 2 hosts: canceled (canceled): not confirmed, nothing was done\n", asked + "|[resume]"},
		{"interrupted while asking", func(_ *safety.Gate, cancel func()) { time.AfterFunc(50*time.Millisecond, cancel) }, "exe[1-2]", "",
			"wait confirm message=drain 2 hosts: canceled (canceled): context canceled\n", asked + "|\n|[resume]"},
		{"confirmed in advance", func(g *safety.Gate, _ func()) { g.AssumeYes = true }, "exe[1-2]", "",
			"wait confirm message=drain 2 hosts: ok\n", ""},
		{"a dry run", func(g *safety.Gate, _ func()) { g.DryRun = true }, "exe[1-2]", "",
			"wait confirm message=drain 2 hosts: skipped: dry run: nothing was done\n", "Would drain 2 hosts: exe[1-2]\n"},
		{"a protected host", nil, "wlm01", "y\n",
			"wait confirm message=drain 1 host: failed (usage): drain would touch the protected host wlm01; pass --force to do it anyway\n", ""},
		{"no terminal", func(g *safety.Gate, _ func()) { g.Interactive = false }, "exe[1-2]", "y\n",
			"wait confirm message=drain 2 hosts: failed (usage): drain needs a confirmation but there is no terminal to ask on; pass -y to confirm in advance\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			term := &terminal{}
			c := &progresstest.Capture{}
			bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c, term}})
			ctx, cancel := context.WithCancel(progress.WithBus(context.Background(), bus))
			defer cancel()

			g, _ := gate(t, tc.answer)
			g.Out, g.Context = term, ctx
			if tc.answer == "" {
				stdin, typing := io.Pipe()
				t.Cleanup(func() { _ = typing.Close() })
				g.In = stdin
			}
			if tc.setup != nil {
				tc.setup(g, cancel)
			}
			_ = g.Confirm(action("drain", tc.nodes))
			bus.Close()
			progresstest.Check(t, c.Events())
			if got := c.Tree(); got != tc.want {
				t.Errorf("progress:\n%s\nwant:\n%s", got, tc.want)
			}
			if got := term.String(); got != tc.shown {
				t.Errorf("the terminal saw %q, want %q", got, tc.shown)
			}
		})
	}
}
