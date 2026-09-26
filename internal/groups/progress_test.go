// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package groups_test

import (
	"context"
	"slices"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/groups"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// Each lookup a resolver makes is reported once, as a hidden call that
// says where its answer came from: a command it sent, the disk cache, or
// the configuration, which says nothing. A lookup answered from memory is
// not reported again. A source without the group has answered, so its
// lookup ends well; one that could not be asked fails as its host did.
func TestEveryLookupIsReportedOnce(t *testing.T) {
	t.Parallel()
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	rec := &transport.Recorder{Reply: func(target transport.Target, req transport.Request) (*transport.Result, error) {
		switch {
		case slices.Contains(req.Argv, "down"):
			return transport.ExitResult(target, 255, "", "ssh: connect to host login: Connection refused\n"), nil
		case slices.Contains(req.Argv, "%R"):
			return &transport.Result{Stdout: "batch\n"}, nil
		}
		return &transport.Result{Stdout: "exe[1-2]\n"}, nil
	}}
	opts := testOptions(t, rec, t.TempDir())
	opts.Context = progress.WithBus(context.Background(), bus)
	first, second := groups.New(opts), groups.New(opts)

	for _, lookup := range []func() error{
		func() error { _, err := first.Resolve("slurm", "batch"); return err },
		func() error { _, err := first.Resolve("slurm", "batch"); return err },
		func() error { _, err := second.Resolve("slurm", "batch"); return err },
		func() error { _, err := first.Resolve("", "exe"); return err },
		func() error { _, err := first.All("slurm"); return err },
		func() error { _, err := first.List("slurm"); return err },
	} {
		if err := lookup(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.Resolve("static", "nosuch"); err == nil {
		t.Error("a group the source lacks was found")
	}
	if _, err := first.Resolve("slurm", "down"); err == nil {
		t.Error("a source that could not be asked answered")
	}
	bus.Close()
	progresstest.Check(t, c.Events())

	want := `call list the groups of @slurm cache=miss [hidden]: ok
call resolve @inventory:exe [hidden]: ok
call resolve @slurm:* cache=miss [hidden]: ok
call resolve @slurm:batch cache=disk [hidden]: ok
call resolve @slurm:batch cache=miss [hidden]: ok
call resolve @slurm:down cache=miss [hidden]: failed (transport): source "slurm": login (login.example.org): ssh: connect to host login: Connection refused
call resolve @static:nosuch message=no such group [hidden]: ok
`
	if got := c.Tree(); got != want {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want)
	}
}
