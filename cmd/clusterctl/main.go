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
	// The first interrupt cancels the work in progress so that remote
	// commands are given the chance to stop; a second one is left to the
	// default handler, which ends the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Execute(ctx))
}
