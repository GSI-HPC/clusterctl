<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Requirements

What clusterctl has to do, and where each requirement is met. The numbering is
this project's own; it is a restatement of the jobs and findings of the review
that preceded the rewrite, not a copy of its numbering.

A requirement is **met** when a command implements it and a test covers it,
**partial** when it is implemented but can only be exercised against real
hardware, and **deferred** when it is deliberately not done.

## Reach

| | Requirement | Where | State |
| --- | --- | --- | --- |
| R01 | Reach every infrastructure host by role name | `cli/login.go`, `app.Role` | met |
| R02 | Roles carry their own account, agent forwarding and jump host | `transport/sshconfig.go` | met |
| R03 | Check every connection against one host key file | `transport/sshconfig.go` | met |
| R04 | Collect, compare and refresh host keys without external tools | `hostkeys` | met |
| R05 | Never lose an entry when two administrators write the file at once | `fileutil.Update` | met |
| R06 | Keep the system and user ssh configuration in effect | generated `Include` of `~/.ssh/config` and `/etc/ssh/ssh_config` | met |
| R07 | Work with OpenSSH 8.0 through current | `PubkeyAcceptedKeyTypes` | met |
| R08 | Reach networks behind a gateway through sshuttle profiles | `tunnel` | met |
| R09 | Multiplex connections for hubs but never per node | `HostRole.ControlMaster` | met |

## Select and fan out

| | Requirement | Where | State |
| --- | --- | --- | --- |
| R10 | Parse ClusterShell node set syntax | `nodeset` | met |
| R11 | Fold and expand idempotently | `nodeset/fold.go` | met |
| R12 | Set operations evaluated left to right | `nodeset/parse.go` | met |
| R13 | Groups from node attributes, tables and commands | `groups` | met |
| R14 | Cache group lookups that cost a round trip | `groups` | met |
| R15 | Run one command on many nodes in parallel, bounded | `fanout` | met |
| R16 | Preserve the argument vector exactly | `shellquote` | met |
| R17 | Enforce a command timeout on the node | `transport.RemoteCommand` | met |
| R18 | Report every node, not just the first failure | `fanout` | met |
| R19 | Collapse identical answers | `fanout.GroupByOutput` | met |
| R20 | Copy files to and from many nodes | `cli/copy.go` | met |
| R21 | Offload very large sets to clush on a hub | nothing yet | deferred |

## Operate hardware

| | Requirement | Where | State |
| --- | --- | --- | --- |
| R22 | Read and change power state | `cli/bmc.go` | met |
| R23 | Prefer Redfish, fall back to IPMI | `app.BMCTransports`, `cli/bmcpower.go` | met |
| R24 | Run the IPMI tools on a host that can reach the service network | `ipmi` | met |
| R25 | Keep the password out of the remote argument vector | `ipmi` | met |
| R26 | Pin the service processor certificate and refuse a change | `redfish/pins.go` | met |
| R27 | Ask a machine which reset types it accepts | `redfish/system.go` | met |
| R28 | Never retry an action | `redfish` | met |
| R29 | Per-vendor profiles for firmware that differs | `VendorProfile` | met |
| R30 | Read and set the boot source override | `cli/bmc.go` | met |
| R31 | Sweep the service processors for a ping | `cli/bmc.go` | partial |
| R32 | Reach rack power distribution units | `cli/bmc.go` | partial |
| R33 | Check fabric port state without the node being up | `cli/fabric.go` | partial |
| R34 | Read fabric error counters | `cli/fabric.go` | partial |
| R35 | Read and set adapter firmware settings | `cli/fabric.go` | partial |
| R36 | Collect a hardware inventory | `cli/node.go` | met |

## Provision

| | Requirement | Where | State |
| --- | --- | --- | --- |
| R37 | Parse the DHCP configuration rather than grep it | `dhcp` | met |
| R38 | Report address, hardware address, client identifier and boot file | `cli/dhcp.go` | met |
| R39 | Read the DHCP log and capture DHCP traffic | `cli/dhcp.go` | partial |
| R40 | Set and remove a PXE boot path for a node set | `cli/boot.go` | met |
| R41 | Derive the boot path from cluster rules | `inventory.BootPath` | met |
| R42 | Refuse a node matched by two boot path rules | `inventory.BootPath` | met |
| R43 | Compute the GRUB file name from the node address | `cli/boot.go` | met |
| R44 | Read the PXE service log and update it from version control | `cli/boot.go` | partial |
| R45 | Decrypt age encrypted secrets without writing plaintext to disk | `secrets` | met |
| R46 | Write secrets onto nodes with owner and mode | `cli/provision.go` | met |
| R47 | Point a node at a configuration archive and run the client | `cli/provision.go` | met |
| R48 | Reinstall a node set end to end | `cli/provision.go` | met |
| R49 | Resolve everything before changing the first machine | `cli/provision.go` | met |

## Slurm

| | Requirement | Where | State |
| --- | --- | --- | --- |
| R50 | List nodes with state and drain reason | `slurm` | met |
| R51 | Node sets by state | `slurm.NodeSet` | met |
| R52 | Drain with a mandatory reason, and resume | `slurm` | met |
| R53 | Read the queue and the accounting database | `slurm` | met |
| R54 | Count jobs per user | `cli/slurm.go` | met |
| R55 | List and create accounts, coordinators and fair share | `slurm` | met |
| R56 | Associate users, checked against the directory with getent | `slurm` | met |
| R57 | Parse the parsable output, never the display output | `slurm` | met |
| R58 | Refuse a power action on a node running a job | `slurm/jobcheck.go`, `cli/bmc.go` | met |

## Everything else

| | Requirement | Where | State |
| --- | --- | --- | --- |
| R59 | One layered configuration for several clusters and sites | `config` | met |
| R60 | Validate each layer before merging, at the line it was written | `config/schema.go` | met |
| R61 | Say which layer set each value | `config.Tree` | met |
| R62 | Publish a JSON Schema for editors | `cli/config.go` | met |
| R63 | One output layer: table, wide, json, yaml, nodeset, name, jsonpath, jq | `output` | met |
| R64 | Fixed exit codes | `exitcode` | met |
| R65 | Preview, confirm and dry run for everything destructive | `safety` | met |
| R66 | Protected hosts | `safety` | met |
| R67 | Shell completion | cobra, `cli/helpers.go` | met |
| R68 | Check an installation before it is needed | `cli/doctor.go` | met |
| R69 | A static binary with no runtime dependencies of its own; the programs it runs, ssh, sshuttle and sops, are the site's (ADR 0004, 0019) | `CGO_ENABLED=0` | met |
| R70 | Keep secrets in sops encrypted documents and check references without a key | `config/secretdoc.go` | met |
| R71 | Decrypt with the workstation's age identities or with the keys sops finds | `secrets/sopscmd.go` | met |

## Deferred, and why

**R21, offloading a very large fan-out to `clush` on a hub.** The executor
bounds concurrency, but nothing offloads a set, and the configuration has no
setting for it: a setting that was accepted and did nothing would let a large
fan-out run from the workstation while the site believed otherwise. It
matters above a few hundred nodes, it needs a cluster of that size to test
honestly, and the wrong implementation is worse than none.

**Compatibility with the old command names.** No `cluster-*` shims and no
importer for the old configuration formats. This was decided deliberately: the
old flags are the inconsistency the rewrite exists to remove, and carrying them
would carry that too. [migration.md](migration.md) maps every old command and
setting to its replacement by hand.
