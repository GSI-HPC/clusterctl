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
| `internal/fanout` | Working on many targets at once, bounded and in order, or in batches with a pause between them, and reporting the work as it goes. |
| `internal/progress` | Reporting the work under way: its steps, targets and calls as spans carried in the context, and every change to one as an event for a display or an agent. |
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
| `internal/progress/display` | Drawing the progress of a command on the terminal of its standard error, and the writers that keep the command's own output clear of it. |
| `internal/version` | The build provenance, which comes from the signed tag or the VCS stamps. |

## Dependency direction

`cli` and `mcpserver` depend on `app`, and `cli` on the subsystems it drives
too; `app` depends on everything else. A subsystem depends on the model, on
`transport` and on the helpers beside it, `output` for escaping among them. No
subsystem imports `config`: `app` hands each the typed `v1alpha1` values it
needs. And none imports `cli` or `app`, so a subsystem can be exercised in a
test without a command tree.

`progress` is a leaf that every layer may report through. It depends on
nothing of clusterctl's but `exitcode`, and `output` for escaping, so
`fanout`, `transport`, `redfish`, `credentials`, `secrets`, `safety` and `app`
can each start and end the spans of their work, and lend the terminal to a
question, without a cycle
([ADR 0021](adr/0021-progress-as-our-own-events.md)). `progress/display`
depends on `progress` alone, and only `cli` uses it.

Text that came from a node, a BMC, Slurm, a group source or an agent is
escaped with `output.EscapeText`, or `output.EscapeCell` where it has to stay
on one line, before it reaches a terminal. There is no other escaper: a
package that quotes such text in an error, as the group resolver does, uses
the same helper, and `cli` escapes every error it prints once more on the way
out, which changes nothing in text that is already escaped. Every line of
output and every error a progress display may draw goes through
`progress.Sanitize`, which applies carriage returns the way a terminal would
and then escapes what is left with `EscapeCell`.

The transport is reached through the `transport.Runner` interface everywhere
except in the commands that open an interactive session. That is what lets a
dry run swap in a recorder and a test drive the whole program without a
cluster.

Every leaf command but an interactive one runs in a progress span of its own,
which `cli` opens before the command context is built, so that
`App.Context()`, the gate and the group resolver carry it and whatever the
command reports nests under it. `app.New` wraps both runners in
`transport.Traced`, which reports every request as an `ssh` call under the
span its context carries: a dry run's recorded requests end skipped, and its
lookups through `ReadRunner`, which reach the host, end as they came back.
`Client.Run` cuts what ssh prints into lines as it arrives, before the bound
on the output drops any of it: a display is shown them where a step shows
lines, as exec's does, and a parser is handed those that ended through
`Request.OnLine`, so that it can end a target before the command has. Standard
input, which carries secrets, is never looked at. A Redfish request and a copy
are calls too, and so, hidden unless they fail or take long, are the plumbing:
a credential read, a file read from a host, a group lookup and a decryption,
each saying where its answer came from but never what it was. The
confirmation is a wait, and a display is off the terminal while its question
is asked and answered, and so it is while a password is asked for, a
credential helper that can reach the terminal runs, or sops looks for its
keys there.

What a command's progress looks like is `cli`'s choice, by `--progress` and
`CLUSTERCTL_PROGRESS`: the counter of `progress/display`, drawn only on a
standard error that is a terminal, or nothing. While it is drawn, the
command's streams, and cobra's, are wrapped in writers that take its line off
before they write and pass what they are given on unchanged; the wrapping is
done before the command context is built, which copies the streams, and only
then, so without a display nothing stands between a command and its streams.
The counter counts the targets of each step the way `progress.Tally` does,
which is also what `progresstest.Check` holds the events to. A Bus that comes
with the context, as a test's or an MCP call's does, draws nothing of its own,
and under `clusterctl mcp` the flag and the variable are not read.

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
is much slower than a node and a wide fan-out to them achieves nothing. A
processor also has few connections to give, so a client closes its connection
once its request is answered, and any it has left idle for as long as a
request may take, rather than keep it for a request that may not come. The
names `dns lookup` resolves are bounded by `services.dns.maxConcurrent`, since
a site's resolver may limit how fast it is asked. Sessions to one
infrastructure host, such as the roles `doctor --remote` asks, are bounded by
`fanout.PerHost`, four, through `fanout.Hosts`, which counts a jump host on
the way as a host too. The MCP server works on two tool calls at once, so that
an agent sending calls side by side does not multiply these bounds.

Every such pool is one loop, `fanout.Each`, which starts nothing once the
command is interrupted, and `fanout.Map` runs any kind of work on it, the
executor's among them, an item that needs a place on a host as well waiting
for it queued. Each kind of work has a bound of its own, which
`fanout.max` in the configuration does not change and a lower `--fanout`
lowers, through `App.Bound`
([ADR 0022](adr/0022-bounded-pools-and-power-batches.md)). A power-on and a
power cycle are sent in batches by `fanout.Batches`, the set split evenly, one
batch after the other with a pause between, and a batch with a failure stops
the run. A pool on `Map` reports its work as the spans of `internal/progress`:
a step, with every target queued before the first one runs and each ended
before it gives its place to the next, so that a display never counts more
running than the bound, and has counted every target, those an interrupt left
out among them, and those left out on purpose, which end skipped, by the time
the step ends. `Batches` reports each batch the same way, all of them
announced before the first is sent, and the pauses between them as waits.

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
