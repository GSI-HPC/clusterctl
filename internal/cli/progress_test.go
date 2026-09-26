// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"testing"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// A command is the root of what it reports, however its function was
// written: through r.run, with a function of its own, or as cobra's help.
// What the command does nests under it, its span ends as report reads the
// error, and a dry run is marked as one, with what it only recorded
// skipped.
func TestEveryCommandIsTheRootOfItsProgress(t *testing.T) {
	hw := &transport.Recorder{ByTarget: map[string]*transport.Result{
		"exe0001": {Stdout: "Vendor: Example\n"},
	}}
	for _, tc := range []struct {
		name     string
		recorder *transport.Recorder
		args     []string
		code     int
		want     string
	}{
		{"a command with a function of its own", nil, []string{"version"}, exitcode.OK, `
command version: ok
`},
		{"cobra's help", nil, []string{"help", "node"}, exitcode.OK, `
command help: ok
`},
		{"a command that fails", nil, []string{"node", "fqdn", "-n", "exe[1-"}, exitcode.Usage, `
command node fqdn: failed (usage): unbalanced [ in "exe[1-"
`},
		{"its calls nest under it", hw, []string{"node", "hw", "-n", "exe1"}, exitcode.OK, `
command node hw: ok
  step run total=1 limit=24 [fold]: ok
    target exe0001: ok
      call ssh node={} host={} timeout=10m0s exit=0: ok
`},
		{"a dry run", hw, []string{"node", "hw", "-n", "exe1", "--dry-run"}, exitcode.OK, `
command node hw [dry-run]: ok
  step run total=1 limit=24 [fold]: ok
    target exe0001: ok
      call ssh node={} host={} timeout=10m0s [dry-run]: skipped: dry run: not sent
`},
		{"a dry run the gate stops", nil, []string{"exec", "-n", "exe1", "--dry-run", "--", "true"}, exitcode.OK, `
command exec [dry-run]: ok
  wait confirm message=run a command on 1 host: skipped: dry run: nothing was done
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, tree := watch(t)
			_, err := run(t, harnessOptions{ctx: ctx, recorder: tc.recorder}, tc.args...)
			wantCode(t, err, tc.code)
			if got := tree(); got != tc.want[1:] {
				t.Errorf("progress:\n%s\nwant:\n%s", got, tc.want[1:])
			}
		})
	}
}

// A command interrupted while it ran ends canceled, whatever the error it
// failed with says, as report tells it.
func TestAnInterruptedCommandEndsCanceled(t *testing.T) {
	watched, tree := watch(t)
	ctx, cancel := context.WithCancel(watched)
	defer cancel()
	rec := &transport.Recorder{Reply: func(t transport.Target, _ transport.Request) (*transport.Result, error) {
		cancel()
		return transport.ExitResult(t, 255, "", "Connection closed\n"), nil
	}}
	_, err := run(t, harnessOptions{ctx: ctx, recorder: rec}, "node", "hw", "-n", "exe1")
	if err == nil {
		t.Fatal("node hw succeeded")
	}
	want := `command node hw: canceled (canceled): 1 of 1 hosts failed: exe0001
  step run total=1 limit=24 [fold]: failed (transport): 1 of 1 failed: exe0001
    target exe0001: canceled (canceled): context canceled
      call ssh node={} host={} timeout=10m0s exit=255: failed (transport): {} ({}): Connection closed
`
	if got := tree(); got != want {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want)
	}
}

// An interactive command hands the terminal to another program, so it
// reports nothing, not even itself.
func TestAnInteractiveCommandReportsNothing(t *testing.T) {
	ctx, tree := watch(t)
	if _, err := run(t, harnessOptions{ctx: ctx}, "login", "--dry-run", "install", "--", "uptime"); err != nil {
		t.Fatalf("login --dry-run failed: %v", err)
	}
	if got := tree(); got != "" {
		t.Errorf("login reported:\n%s", got)
	}
}

// Every leaf runs in a span, whether it has a RunE or a Run, but for an
// interactive one; a command that only holds others runs in none, since
// it only prints its help.
func TestEveryLeafButAnInteractiveOneRunsInASpan(t *testing.T) {
	ctx, tree := watch(t)
	r := &root{ctx: ctx}
	ran := map[string]bool{}
	note := func(c *cobra.Command, _ []string) error {
		ran[c.Name()] = true
		return nil
	}
	top := &cobra.Command{Use: "top"}
	group := &cobra.Command{Use: "group", RunE: note}
	group.AddCommand(&cobra.Command{Use: "withrune", RunE: note})
	group.AddCommand(&cobra.Command{Use: "withrun", Run: func(c *cobra.Command, args []string) { _ = note(c, args) }})
	group.AddCommand(&cobra.Command{Use: "shell", RunE: note,
		Annotations: map[string]string{safety.EffectAnnotation: string(safety.EffectInteractive)}})
	top.AddCommand(group)
	traceLeaves(top, r)

	for _, args := range [][]string{{"group"}, {"group", "withrune"}, {"group", "withrun"}, {"group", "shell"}} {
		top.SetArgs(args)
		if err := top.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		r.ctx = ctx
	}
	if len(ran) != 4 {
		t.Errorf("ran %v, want all four", ran)
	}
	want := `command group withrun: ok
command group withrune: ok
`
	if got := tree(); got != want {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want)
	}
}
