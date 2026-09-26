// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package credentials_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/credentials"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
)

// A read is reported once, by the credential's name and the kind of its
// source, however many callers ask at once, and a failure once too; the
// lookups answered from memory are not reported, and nothing says what was
// read.
func TestAReadIsReportedOnceWithoutItsValue(t *testing.T) {
	t.Parallel()
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	ctx := progress.WithBus(context.Background(), bus)
	r := &credentials.Resolver{
		Credentials: map[string]v1alpha1.Credential{
			"bmc":   {Username: "admin", Password: v1alpha1.PasswordSource{Prompt: true}},
			"pdu":   {Username: "apc", Password: v1alpha1.PasswordSource{FromEnv: "PDU_PASSWORD"}},
			"empty": {Username: "x", Password: v1alpha1.PasswordSource{FromEnv: "UNSET"}},
		},
		Env:    func(k string) string { return map[string]string{"PDU_PASSWORD": "s3cret-pdu"}[k] },
		Prompt: func(string) (string, error) { return "s3cret-typed", nil },
	}
	var wg sync.WaitGroup
	for range 8 {
		for _, name := range []string{"bmc", "pdu", "empty"} {
			wg.Go(func() { _, _ = r.Get(ctx, name) })
		}
	}
	wg.Wait()
	bus.Close()
	events := c.Events()
	progresstest.Check(t, events)

	want := `call credential bmc source=prompt [hidden]: ok
call credential empty source=env [hidden]: failed (usage): credential "empty" reads UNSET, which is not set
call credential pdu source=env [hidden]: ok
`
	if got := c.Tree(); got != want {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want)
	}
	for _, e := range events {
		if text := e.Name + e.Message + e.Err + e.Source; strings.Contains(text, "s3cret") {
			t.Errorf("event %d carries a password: %+v", e.Seq, e)
		}
	}
}

// suspending is a display that logs when it is taken off the terminal and
// put back, among what else the test logs.
type suspending struct {
	mu  sync.Mutex
	log []string
}

func (s *suspending) Handle(progress.Event) {}
func (s *suspending) Suspend()              { s.add("suspend") }
func (s *suspending) Resume()               { s.add("resume") }

func (s *suspending) add(what string) {
	s.mu.Lock()
	s.log = append(s.log, what)
	s.mu.Unlock()
}

// A password prompt, and a helper that can reach the terminal, run with the
// displays off it; a helper started in a session of its own, which has no
// terminal to ask on, and a source that asks nobody, leave them where they
// are.
func TestTheDisplaysLeaveTheTerminalForAQuestion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		source     v1alpha1.PasswordSource
		noTerminal bool
		want       string
	}{
		{"a prompt", v1alpha1.PasswordSource{Prompt: true}, false, "suspend ask resume"},
		{"a helper at a terminal", v1alpha1.PasswordSource{Command: []string{"echo", "from-helper"}}, false, "suspend resume"},
		{"a helper without one", v1alpha1.PasswordSource{Command: []string{"echo", "from-helper"}}, true, ""},
		{"a variable", v1alpha1.PasswordSource{FromEnv: "BMC_PASSWORD"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			display := &suspending{}
			bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{display}})
			defer bus.Close()
			r := &credentials.Resolver{
				Credentials: map[string]v1alpha1.Credential{"bmc": {Username: "admin", Password: tc.source}},
				Env:         func(string) string { return "from-env" },
				Prompt: func(string) (string, error) {
					display.add("ask")
					return "typed", nil
				},
				NoTerminal: tc.noTerminal,
			}
			if _, err := r.Get(progress.WithBus(context.Background(), bus), "bmc"); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(display.log, " "); got != tc.want {
				t.Errorf("the display saw %q, want %q", got, tc.want)
			}
		})
	}
}
