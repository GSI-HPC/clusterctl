<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0022 — Bounded pools, and power actions in batches

Status: accepted

## Context

A command on many hosts works on them side by side, and each kind of work
meets its own limit at the far end. By default sshd starts to drop
connections once ten are waiting to authenticate, and a tunnel or a jump host
gives up sooner. A service processor's web server is far slower than a node and has
few connections to give. A site's resolver was never asked for more than one
name at a time. And the MCP server runs a whole command for every tool call,
so calls side by side multiply every one of these.

The executor, the Redfish fan-out and the host key scans each kept a
semaphore of their own until `5413ac7`, and the scan went on dialling every
host after an interrupt. `fanout.Each` has been the one loop since. Other
loops still reach one host at a time and are to become pools: the names of
`dns lookup`, the processors ipmitool reaches from the role `bmc.ipmi.via`
names, the checks of `doctor --remote`.

`--fanout` is the command line's layer of `fanout.max`. It bounds ssh and
scp to nodes and the host key scans; the Redfish fan-out is bounded by
`bmc.redfish.maxConcurrent`, whatever `--fanout` says.

A rack powering on at once trips its breaker, so a power-on is sent in
batches. A design for the pools proposed batching `reset`, `bmc redfish
post` and the reset of a reinstall too, carrying on after a batch that
partly failed, and cutting a batch down to the pool's limit.
`d6cbf58` had decided otherwise, and this record keeps those rules.

## Decision

- **One loop.** Every pool runs on `fanout.Each(ctx, n, limit, work)`: at
  most `limit` calls at a time, none started once the context has ended,
  and a place taken as it ends is given back, since `select` picks either
  of two cases that are ready. It returns once every call has returned, and
  the caller tells what was left out from what it did not record. Results
  are kept in the order the targets were given. A panic in one call becomes
  that target's failure, through `fanout.Recovered`, with its stack in the
  front end's diagnostics.
- **A bound for each kind of work.** One a site can set has a default of its
  own in `defaults.yaml`, not derived from `fanout.max`:

  | Work | Bound | Default |
  | --- | --- | --- |
  | ssh and scp to nodes, host key scans | `fanout.max` | 16 |
  | Redfish requests | `bmc.redfish.maxConcurrent` | 8 |
  | ipmitool, for the processors it reaches from `bmc.ipmi.via` | `bmc.ipmi.maxConcurrent` | 8 |
  | The names of `dns lookup` | `services.dns.maxConcurrent` | 16 |
  | Sessions to one role host, such as `doctor --remote`'s | `fanout.PerHost` | 4 |
  | MCP tool calls, across the server | fixed | 2 |

- **`--fanout` lowers the other bounds and never raises them.** Given on the
  command line, it sets `fanout.max`, and it lowers the Redfish, IPMI and
  DNS bounds to its value where that is lower. `fanout.max` set in a
  configuration does not change them. MCP does not take `--fanout`.
- **Only a power-on and a power cycle are sent in batches.** The batch is
  `--batch`, or `safety.powerOnBatch`. The set is split evenly into as few
  batches as that allows, so ten nodes at 8 go as 5 and 5, not 8 and 2. The
  batches are sent one after the other: the next starts once every request
  of the one before has returned and `--stagger`, or
  `safety.powerOnStagger`, has passed, and both are announced on standard
  error. A batch with a failure stops the run, and the nodes of the later
  batches are reported as not tried; an interrupt stops it too, and they
  are reported as not sent. `reset`, `bmc redfish post` and the reset of a
  reinstall are sent to the whole set as one fan-out.
- **An action is sent over a second transport only when the first never
  reached the processor.** Each node is tried over the first transport of
  its order. A read that failed is tried over the next; an action only when
  its name did not resolve or nothing accepted the connection. Nothing is
  tried again after an interrupt or a usage error, or when a processor
  presented another certificate than the one recorded: it may not be the
  processor, and its account goes to it over no protocol.
- **IPMI once per account.** The nodes that share an account share one IPMI
  backend and one run of it, with the set of their processors, so one
  vendor's password is never offered to another vendor's processors.
- **What a pool reports**, through `internal/progress`
  ([ADR 0021](0021-progress-as-our-own-events.md)): a step with its Total and
  its Limit; every target queued before the first one runs; each marked
  running when it takes its place, and ended before it gives the place up,
  so no display counts more running than the limit; those never started
  ended as canceled, so the count reaches the Total; and the step ended once
  the last has. Batches are spans too, all of them queued at the start, with
  a wait for each pause, and a batch that is not tried ends skipped and
  counts as its Total.
- **One exit code.** The failures of a pool become the command's exit code
  through `exitcode.Worst` ([ADR 0020](0020-one-exit-code-rule-for-many-hosts.md)).

## Why

- A bound belongs to the far end. A site that sets `fanout.max: 4` for a
  fragile jump host does not want its service processors, which it reaches
  directly, slowed to 4, and one that sets 64 for its nodes does not want
  them asked 64 at a time. So `fanout.max` bounds only the connections to
  nodes it was set for.
- `--fanout` is what an administrator types to be gentle, and `--fanout 1`
  is what they type to see one host at a time. Lowering every bound keeps
  both meaning what they say; raising them would make one flag given for
  ssh reach the processors too.
- One loop decides once when to stop starting work, so an interrupt stops
  every pool the same way.
- A power-on and a power cycle draw the inrush current of every machine at
  once. A reset restarts a machine that is running, which stays powered
  through it. `bmc redfish post` sends what the administrator wrote, and
  batching every POST would slow the many that are not about power for the
  few that are, which `power on` and `power cycle` are there for.
- An even split keeps the last batch from being a stray node, and a batch
  starts only once the one before has answered, since a slow processor may
  land its power-on late, into the next batch's.
- A failed batch may be the breaker the batches are there to protect.
  Stopping leaves the rest off, which is safe, and the result names them.
- An action that may have reached the processor may have been carried out,
  and a second one over IPMI would carry it out again.

## Costs

- A power-on of 480 nodes in batches of 8 pauses 59 times, five minutes at
  5 s, however fast the processors answer. That is the point, and
  `--stagger` shortens it.
- A failure in the first batch leaves the rest of the set off until the
  command is run again for them.
- On some firmware a reset powers on a machine that is off. Reset the
  machines that are off with `power on`, which is batched, or `power
  cycle`.
- `--fanout` means two things: it sets the bound of ssh, and lowers the
  others. `--fanout 64` does not reach the processors.
- Six bounds to know instead of one. Each has a default that suits the far
  end it protects, and the manual names them where they apply.

## Reconsider when

- a site's breakers trip on a reset: then `reset` is batched too;
- nodes that share a processor, as in a multi-node chassis, get systems of
  their own, when a bound per processor becomes worth having;
- or a pool's far end is a shared service that needs a bound across
  commands, which a bound per command cannot give.
