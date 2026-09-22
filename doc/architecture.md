<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Architecture

clusterctl is one binary. It has no daemon, no state beyond a cache and a few
files under `$XDG_STATE_HOME`, and it never needs to be installed on the hosts
it administers.

## The shape of a command

Every command follows the same path:

```
cmd/clusterctl        the process: signals, exit code
  internal/cli        the command tree: flags, arguments, help
    internal/app      the resolved context: configuration, transport, gate
      internal/config the layered configuration and its provenance
      internal/...    the subsystem the command needs
        internal/transport  the ssh client
    internal/output   the format the result is printed in
```

A command never reads the configuration, builds an ssh command line or formats
its own output. It asks the `app.App` it is given for a node set, for targets
and for an executor, does its work, and hands one `output.Result` back. That is
why `-o json`, `--dry-run` and the confirmation gate behave identically in
every command rather than in the ones that remembered to implement them.

## Packages

### The model

| Package | Owns |
| --- | --- |
| `nodeset` | The node set language: parsing, folding, expansion, set operations, groups. The only package outside `internal/`. |
| `internal/apis/v1alpha1` | The configuration document kinds and the effective configuration they merge into. |
| `internal/config` | Finding, validating, merging and resolving configuration, and remembering where every value came from. |
| `internal/inventory` | What is known about the nodes: attributes, racks, addresses, boot paths. |
| `internal/naming` | Turning a short node name into a host name and a service processor name. |
| `internal/groups` | Resolving `@group` references from tables, node attributes or commands. |

### Getting things done

| Package | Owns |
| --- | --- |
| `internal/transport` | Driving the OpenSSH client, and generating the configuration it runs with. |
| `internal/shellquote` | Rendering an argument vector so a remote shell reproduces it exactly. |
| `internal/fanout` | Running one request on many targets, bounded and in order. |
| `internal/safety` | Deciding whether a destructive action may proceed. |
| `internal/fileutil` | Writing files atomically and under a lock. |

### Subsystems

| Package | Owns |
| --- | --- |
| `internal/redfish` | The Redfish client, its certificate pinning and its reset semantics. |
| `internal/ipmi` | The FreeIPMI and ipmitool backends, run on a host that can reach the service network. |
| `internal/credentials` | Resolving an account and its password from a configured source. |
| `internal/secrets` | Decrypting age encrypted files and inline values into memory, and encrypting values to the site's recipients. |
| `internal/slurm` | Reading and changing the state of the workload manager. |
| `internal/dhcp` | Parsing an ISC dhcpd configuration. |
| `internal/hostkeys` | Reading, writing and collecting SSH host keys. |
| `internal/tunnel` | Managing the sshuttle profiles of a site. |

### Presentation

| Package | Owns |
| --- | --- |
| `internal/output` | Table, JSON, YAML, node set, name, JSONPath and jq rendering. |
| `internal/exitcode` | The exit codes the command line contract fixes. |
| `internal/version` | The build provenance, which comes from the signed tag or the VCS stamps. |

## Dependency direction

`cli` depends on `app`, `app` depends on everything else, and the subsystems
depend only on `transport`, `config` and the model. No subsystem imports `cli`
or `app`, so a subsystem can be exercised in a test without a command tree.

The transport is reached through the `transport.Runner` interface everywhere
except in the commands that open an interactive session. That is what lets a
dry run swap in a recorder and a test drive the whole program without a
cluster.

## What runs where

clusterctl runs on an administrator's workstation. Almost everything it does
happens somewhere else:

- **On the workstation**: parsing, merging, node set arithmetic, naming, age
  decryption, Redfish over HTTPS, and the ssh and sshuttle clients.
- **On an infrastructure host**: the IPMI tools, the Slurm clients, the fabric
  diagnostics, the DHCP and PXE service files, and `fping`. These need a route
  into a network the workstation cannot reach, or a client it should not have
  to install.
- **On the nodes**: whatever a command was asked to run, the adapter tools, and
  the configuration management client.

Which host role a subsystem runs on is configuration, not code. A site with a
flat network can set `bmc.ipmi.via` to a role that is its own workstation and
nothing else changes.

## Concurrency

Work on many hosts is bounded by `fanout.max`, which defaults to 16. Results
come back in the order the targets were given, whatever order they finished in,
so two runs of the same command produce the same output. Redfish requests are
bounded separately by `bmc.redfish.maxConcurrent`, because a service processor
is much slower than a node and a wide fan-out to them achieves nothing.

Cancellation travels through one `context.Context` from the signal handler in
`main` down to every request. A second interrupt is left to the default
handler, so a command that is stuck in a syscall can still be killed.
