// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Command clusterctl administers HPC clusters: it reaches the hosts of a
// site, selects and drives nodes, operates their service processors,
// provisions them and administers Slurm.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/GSI-HPC/clusterctl/internal/cli"
)

func main() {
	ctx, stop := interruptContext()
	code := cli.Execute(ctx)
	stop()
	os.Exit(code)
}

// interruptContext returns a context that the first SIGINT or SIGTERM
// cancels, which stops the work in progress: no new request is started, ssh
// is asked to stop, and the command exits 130. The remote commands already
// running are not signalled; their timeout on the host ends them.
//
// The handler is removed as soon as the first signal arrives, so a second
// one gets the default action and ends the process, even while it waits in
// something that does not watch the context. The context's cause is
// context.Canceled, not the signal, so that every path reports the same
// interruption.
func interruptContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-signals:
		case <-done:
		}
		signal.Stop(signals)
		cancel()
	}()
	return ctx, func() { close(done) }
}
