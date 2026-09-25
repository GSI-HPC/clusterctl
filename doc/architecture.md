<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
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

A command never builds an ssh command line or formats its own output. It asks
the `app.App` it is given for a node set, for targets, for an executor and for
the clients of the subsystems, reads what else it needs from the resolved
configuration, `a.Spec`, does its work, and hands one `output.Result` back.
That is why `-o json`, `--dry-run` and the confirmation gate behave identically
in every command rather than in the ones that remembered to implement them.

`app` is the resolved context and a factory, not a facade: `cli` imports the
subsystems it drives and calls them itself, with what `app` built.

## Packages

### The model

| Package | Owns |
| --- | --- |
| `nodeset` | The node set language: parsing, folding, expansion, set operations, groups. The only package outside `internal/`. |
| `internal/apis/v1alpha1` | The configuration document kinds and the effective configuration they merge into. |
| `internal/config` | Finding, validating, merging and resolving configuration, and remembering where every value came from. |
| `internal/inventory` | What is known about the nodes: attributes, racks, addresses, boot paths. |
| `internal/naming` | Turning a short node name into a host name and a service processor name. |
| `internal/hostname` | Deciding whether a name may be handed to ssh or put into a URL as a host. |
| `internal/exitcode` | The exit codes the command line contract fixes, which every layer that can fail uses to say which one it asks for. |

### Getting things done

| Package | Owns |
| --- | --- |
| `internal/transport` | Driving the OpenSSH client, and generating the configuration it runs with. |
| `internal/shellquote` | Rendering an argument vector so a remote shell reproduces it exactly. |
| `internal/fanout` | Running one request on many targets, bounded and in order. |
| `internal/safety` | Deciding whether a destructive action may proceed. |
| `internal/fileutil` | Writing files atomically and under a lock, and the cache on disk. |

### Front ends

| Package | Owns |
| --- | --- |
| `internal/cli` | The command tree: flags, arguments, help, and the commands themselves. |
| `internal/mcpserver` | Offering the commands to an AI agent over MCP, with changes only through plan and apply. It builds an `app.App` for each call and runs the command tree through a factory `cli` hands it, which keeps the import one way. |

### Subsystems

| Package | Owns |
| --- | --- |
| `internal/redfish` | The Redfish client, its certificate pinning and its reset semantics. |
| `internal/ipmi` | The FreeIPMI and ipmitool backends, run on a host that can reach the service network. |
| `internal/groups` | Resolving `@group` references from tables, node attributes or commands run on a host role, and caching what the commands answered. |
| `internal/credentials` | Resolving an account and its password from a configured source. |
| `internal/secrets` | Decrypting age encrypted files into memory, reading the sops metadata of a Secret document, and having the `sops` command decrypt it into memory. |
| `internal/slurm` | Reading and changing the state of the workload manager. |
| `internal/dhcp` | Parsing an ISC dhcpd configuration. |
| `internal/hostkeys` | Reading, writing and collecting SSH host keys. |
| `internal/tunnel` | Managing the sshuttle profiles of a site. |

### Presentation

| Package | Owns |
| --- | --- |
| `internal/output` | Table, JSON, YAML, node set, name and jq rendering, and escaping untrusted text for a terminal (`EscapeText`, `EscapeCell`). |
| `internal/version` | The build provenance, which comes from the signed tag or the VCS stamps. |

## Dependency direction

`cli` and `mcpserver` depend on `app`, and `cli` on the subsystems it drives
too; `app` depends on everything else. A subsystem depends on the model, on
`transport` and on the helpers beside it, `output` for escaping among them. No
subsystem imports `config`: `app` hands each the typed `v1alpha1` values it
needs. And none imports `cli` or `app`, so a subsystem can be exercised in a
test without a command tree.

Text that came from a node, a BMC, Slurm, a group source or an agent is
escaped with `output.EscapeText`, or `output.EscapeCell` where it has to stay
on one line, before it reaches a terminal. There is no other escaper: a
package that quotes such text in an error, as the group resolver does, uses
the same helper, and `cli` escapes every error it prints once more on the way
out, which changes nothing in text that is already escaped.

The transport is reached through the `transport.Runner` interface everywhere
except in the commands that open an interactive session. That is what lets a
dry run swap in a recorder and a test drive the whole program without a
cluster.

## What runs where

clusterctl runs on an administrator's workstation. Almost everything it does
happens somewhere else:

- **On the workstation**: parsing, merging, node set arithmetic, naming, age
  decryption, Redfish over HTTPS, the ssh and sshuttle clients, and `sops`,
  which decrypts the Secret documents
  ([ADR 0019](adr/0019-decrypt-with-the-sops-command.md)).
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

A panic while one target is worked on is recovered in that target's worker,
since `recover` only reaches its own goroutine, and becomes that target's
failure, with the stack written to the front end's diagnostics: standard
error on the command line, the server's log under `clusterctl mcp`. The other
targets finish, and the command fails rather than the process ending.

Cancellation travels through one `context.Context` from the signal handler in
`main` down to every request. The first SIGINT or SIGTERM cancels it: no new
target is started and no password is read any more, the prompts and the
`--stdin` read stop waiting, ssh is sent SIGTERM and killed if it is still
there five seconds later, and the command exits 130 however the interrupted
work failed. The handler is removed at that moment, so a second interrupt gets
the default action and ends the process, even one stuck in a syscall. The
remote commands already running are not signalled; their timeout on the host
ends them.
