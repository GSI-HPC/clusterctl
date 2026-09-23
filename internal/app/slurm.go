// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/slurm"
)

// Slurm returns a client for the workload manager, which runs its clients on
// the host role the cluster names.
func (a *App) Slurm() (*slurm.Client, error) {
	role := a.Spec.Slurm.Role
	if role == "" {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no host role runs the Slurm clients; set slurm.role in the cluster document")
	}
	target, err := a.Role(role)
	if err != nil {
		return nil, err
	}
	return &slurm.Client{
		Runner:  a.Runner,
		Target:  target,
		Spec:    a.Spec.Slurm,
		Timeout: a.Timeout().Get(),
	}, nil
}
