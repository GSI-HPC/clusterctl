// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

// Package e2e runs the clusterctl binary against a Slurm cluster in Docker,
// created by sind: real OpenSSH, real sshd on every node, a real slurmctld
// and slurmd. Where the other tests assert what would be sent to a node,
// these assert what happened on it.
//
// The build tag keeps the package out of "go test ./..."; "make e2e" runs it
// against the cluster "make e2e-up" creates, and doc/testing.md says what
// it covers.
package e2e
