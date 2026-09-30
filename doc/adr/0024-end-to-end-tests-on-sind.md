<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0024 — End-to-end tests against a sind cluster

Status: accepted

## Context

Every test stopped at the transport. `transport.Recorder` stands in for ssh,
so the command tests assert what would be sent to a node and never what
happened there, and [testing.md](../testing.md) listed a Slurm controller
among what is deliberately not tested. What lies past that point had been
checked only by hand: that the generated ssh configuration and an included
one together reach a host, that a remote shell rebuilds the argument vector,
that `timeout(1)` ends a command on the node, that the parsers read what
sinfo and squeue print, that a changed host key is refused. The review that
preceded the rewrite named a containerised Slurm as a backend worth standing
up.

[sind](https://github.com/GSI-HPC/sind) creates Slurm clusters in Docker,
each node a container with systemd, sshd, munge and the daemons of its role,
and publishes a node image for each supported Slurm release line.
[sind-action](https://github.com/GSI-HPC/sind-action) creates the clusters in
a GitHub Actions job. sind exports, for each realm, an ssh configuration that
reaches the nodes through a relay container with `ProxyCommand`, together
with its key and the host keys it collected.

## Decision

- **A Go package under a build tag.** `e2e/` runs the clusterctl binary,
  built by `make build` in CI, with `-o json` where there is one, and checks
  each effect past clusterctl, with `docker exec` in the node's container.
  The tag `e2e` keeps it out of `go test ./...`; `make lint` and
  golangci-lint build it with the tag.
- **The suite uses a cluster, and does not make one.** sind-action creates it
  in CI and `make e2e-up` on a workstation, from `e2e/testdata/sind-cluster.yaml`:
  a controller, a submitter and three workers in the realm `e2e`. The suite
  fails, rather than skips, when sind or the cluster is not there, as the
  tests of Secret documents do without sops.
- **clusterctl is configured as a site would configure it.** The documents in
  `e2e/testdata/site/` name the cluster's domain, and the workstation document
  the suite writes includes sind's ssh configuration. The site's host key file
  is a copy of the keys sind collected, with strict checking on. Nothing in
  clusterctl knows about sind.
- **The host key commands reach the nodes directly.** They make a handshake of
  their own, which sind's relay does not carry, so CI adds the nodes' names
  to `/etc/hosts` from sind's DNS records, and a Linux host reaches the
  addresses on sind's Docker networks.
- **Each Slurm release line sind publishes an image for.** CI runs the suite
  once for 25.11 and once for 26.05, side by side, with the node image of
  each.
- **sind is pinned.** `mise.toml` names the version, for Linux alone, and CI
  reads it from there, as it reads Go and sops.

## Why

- A Go package is written as the rest of the tests are: table-driven, with
  typed output, and a cleanup that puts the cluster back after a test that
  changed it. A shell script was what the smoke tests in CI are, and they
  assert exit codes and little else.
- A cluster made outside the suite is created once and reused while a test is
  being written, and keeps Docker out of the Go code.
- Going through sind's own ssh configuration, rather than one written for the
  tests, checks the order the generated configuration relies on: its settings
  first, so that the trust settings are clusterctl's, and an included file
  that adds the route, the identity and the account.
- A node image per Slurm release line is the only way to find a change in
  what sinfo or squeue prints before a site that upgraded does.
- Pinning makes a red run a change in this repository and not a new sind.

## Costs

- The suite runs only on Linux with Docker and cgroup v2. A macOS contributor
  runs it in CI.
- Each run pulls the node image and starts five containers with systemd,
  which adds some minutes to every change, twice over, beside the other jobs.
- sind is updated by hand, as [release.md](../release.md) says; Dependabot
  updates sind-action, not the sind it installs.
- What sind does not stand up stays untested past the transport: slurmdbd and
  so the accounting commands, service processors, PXE and the fabric.
- The host key tests need the nodes' names to resolve on the machine running
  them, which on a workstation means `/etc/hosts` or sind's systemd-resolved
  integration.
