---
title: Working with an agent
weight: 8
---

`clusterctl mcp serve` offers clusterctl to an AI agent in an MCP client
such as Claude Code. The agent can read the state of the cluster and propose
changes. It cannot make a change you have not confirmed.

## Setting it up

The server runs on your workstation, as you. It uses your configuration and
your ssh agent, and stays in the context it was started in.

```console
$ claude mcp add clusterctl -- clusterctl mcp serve --context cluster1
```

Start one server per cluster, each with its own `--context`. The server does
not follow a change of `currentContext` while it runs.

## What the agent can do

| Tool | |
| --- | --- |
| `select_nodes` | Resolve a node set expression and show which protected hosts and unknown nodes it contains |
| `describe_nodes` | Names, inventory, Slurm state and drain reason, and running jobs for up to 64 nodes at once |
| `query_slurm` | Nodes, the queue, finished jobs, partitions, or queued jobs per user |
| `read_command` | Any clusterctl command that only reads, for example `dhcp hosts`, `fabric state` or `bmc status` |
| `plan_change` | Prepare a drain or a resume, and show what would happen |
| `apply_plan` | Carry out a plan after you confirm it |

It cannot run commands on the nodes, open shells, or start tunnels.

## How a change is confirmed

Ask the agent to drain a node, and it plans the change first:

```text
drain 3 hosts: exe[0001-0003]
  reason: "ticket 4712: fans"
current state: drained exe0001, mixed exe0002, idle exe0003
warnings: exe0002 run jobs; they keep running and the nodes stay draining until the jobs end
commands: login (login.hpc.example.org): scontrol update 'nodename=exe[0001-0003]' state=drain 'reason=ticket 4712: fans'
```

The node set in the plan is resolved once and does not change afterwards.
Protected hosts are refused, as they are at the terminal, and `--force` is
not available.

When the agent applies the plan, your client asks **you** the question
clusterctl would ask at the terminal: yes or no, or, above
`safety.confirmAbove` hosts, the number of hosts. The agent cannot answer it.
If you decline, nothing is sent. A plan can be applied once and expires after
ten minutes (`--plan-ttl`).

{{< callout type="warning" >}}
Asking you needs a client that supports MCP elicitation. With a client that
does not, applying a plan is refused, and the refusal gives the command line to
run yourself. `--confirm=approval` instead relies on the client asking you
before each `apply_plan` call. Only use it with a client configured to ask
every time; never allow that tool automatically.
{{< /callout >}}

## The audit trail

Every plan, refusal and apply is recorded, one JSON object per line, in
`~/.local/state/clusterctl/mcp/audit.jsonl` (under `$XDG_STATE_HOME` when it
is set).

```console
$ jq -c '[.time, .event, .action, .nodes, .outcome]' ~/.local/state/clusterctl/mcp/audit.jsonl
```
