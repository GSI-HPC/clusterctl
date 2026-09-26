<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# The MCP server

`clusterctl mcp serve` offers clusterctl to an AI agent over the Model
Context Protocol. It runs on an administrator's workstation, over stdio, in
a session the administrator is watching. This document explains how it is
built and why it looks the way it does. How to use it is in the manual,
under Guides → Working with an agent.

## What it had to get right

The command line already has a safety model: a destructive command previews
what it will do and asks, and without a terminal it refuses unless `-y`
answered in advance (see [safety.md](safety.md)). An MCP server has no
terminal. Mapping it onto `-y` would give the agent the power to answer every
question the gate exists to put to a person. So **who answers the gate
mattered more than the shape of the tools**, and every other decision follows
from that one.

## The shapes that were considered

| | Approach | Why not |
| --- | --- | --- |
| A | One tool taking an argument vector, run with `-o json` | The client sees one tool, so it cannot let reads through and ask before writes, and confirmation collapses into `-y`. |
| B | One tool per command, generated from the cobra tree | About 90 tools crowd the agent's context and make it worse at choosing; mixed commands such as `bmc power ACTION` hide six different risks behind one enum; a question like "what is wrong with exe0007" takes five calls. |
| C | A new surface shaped around the agent's questions | Good for the common questions, but either grows to cover every command or leaves the long tail out of reach. |
| **D** | **C for the common questions, plus a read-only way into every other command, and changes only through plan and apply** | Chosen. See [ADR 0014](adr/0014-mcp-plan-and-apply.md). |

## The tools

| Tool | Does | Annotation |
| --- | --- | --- |
| `select_nodes` | Resolve an expression: the folded set, its size, the names when there are few, nodes the inventory does not know, protected hosts in it | read-only |
| `describe_nodes` | Up to 64 nodes in one call: names, inventory entry, Slurm state and reason, jobs on each node; groups on request | read-only |
| `query_slurm` | Nodes, jobs, history, partitions, or a per-user summary, with filters and a row limit | read-only |
| `read_command` | Any command the tree marks read-only, with JSON output | read-only |
| `plan_change` | Resolve, check and preview a change; nothing is sent | read-only |
| `apply_plan` | Carry out a plan once the administrator has confirmed it | destructive |

Two things are deliberately not offered. **Running commands on the nodes**
(`exec`, `copy`) is a root shell on the cluster whose effect cannot be
classified. **Interactive commands** (`login`, the `shell` commands,
`bmc web`, `dhcp capture`, `tunnel start`) need a terminal or keep running.
cobra's `help` and `completion` only read, but they serve the shell rather
than the cluster, so the tree `read_command` uses leaves them out.

### What each command does

`internal/cli/effects.go` is one table that says, for every command, whether
it reads, changes, or is interactive. The tree records it on each command as
the `clusterctl.effect` annotation, and `read_command` runs only commands
marked read. The table is kept in step by a test: a command cannot be added
without an entry, and an entry cannot outlive its command. A command missing
from the table counts as a change.

A command whose effect depends on its arguments is a change. `bmc power
status` only reads, but the table cannot see the argument, and `bmc status`
asks the same question.

`read_command` checks the command on a tree of its own before running it on
another, and refuses the global options the server sets itself: `--config`,
`--context`, `--set`, `--yes`, `--force`, `--fanout`, which would
otherwise override `fanout.max`, and `--progress`: no progress display is
drawn for an agent, and `CLUSTERCTL_PROGRESS` in the server's environment is
ignored too. A `--timeout` may shorten the wait for a host but not lengthen it
beyond the command's default.

A read command still connects somewhere, so the names an agent passes are held
to the same rule as any other selection: a node name has to be a host name.
One beginning with `-` would otherwise reach ssh as an option, and one with a
`:`, `@`, `/`, `?` or `#` would send a Redfish read, and the site's BMC
password, to a host and port of the agent's choosing. See
[node sets](nodeset.md#names-are-host-names).

The names are also held to the site. At a terminal the administrator may name
any host; through `read_command` a node has to be in the inventory, or have a
host name, as the naming rules give it, below one of the site's `domains`.
Otherwise `hostkey scan` could be pointed at any machine the workstation
reaches, and `dns lookup` could carry data out to a resolver in a name. A
group whose members are neither is refused as well.

## Plan and apply

A change is two calls.

**`plan_change`** resolves the node set **once**. The plan keeps the result,
because `@slurm:main` may name different nodes by the time the plan is applied than
when it was read. It then runs `safety.Gate.Preview`, which applies the same
checks as the prompt (protected hosts, nodes the inventory does not know, an
empty selection) and describes the question without asking it. Finally it runs the action against a
`transport.Recorder`, so the plan lists exactly what `apply_plan` will send,
rendered the way `--dry-run` prints it. The plan also carries the current
Slurm state of the nodes, warnings (a drained node's reason is about to be
replaced, jobs keep running), and the equivalent command line.

**`apply_plan`** takes the plan's id and has the agent repeat its nodes and
count. Repeating them puts the hosts into the call itself, so a client that
asks before running a tool shows what the call touches, not just an opaque
id. Then:

1. The configuration is read again and the plan checked against it. The
   pinned context has to name the same cluster, the gate has to read the
   same hosts, and running the action against the recorder has to give the
   same commands to the same host. Anything else is refused and needs a new
   plan: the context may have been pointed at another cluster, or the Slurm
   role at another host, while the plan waited. The gate is re-checked too,
   so a host protected since the plan was made is refused.
2. The gate's question is put to the administrator as an **MCP elicitation**:
   the quoted reason, the context and cluster as they resolve now, the
   summary, and a yes/no, or the host count above `safety.confirmAbove` as it
   reads now, so a threshold lowered while the plan waited applies. The reason
   is the agent's, so it comes first and the server's summary last, next to
   the answer. The client shows it to the person and returns the answer. The
   model neither sees nor writes it.
   `safety.Preview.Accept` judges the answer, which is the rule the terminal
   prompt uses.
3. The plan is taken, so it is applied at most once. The apply is recorded
   in the audit log before anything is sent, and nothing is sent when that
   record cannot be written. The action then runs to its end even if the
   client gives up on the call, since Slurm may already have taken the
   change; it is bounded by its own timeout instead.

The question travels as an input request of the `tools/call` (SEP-2322, the
2026-07-28 protocol). The answer comes back on a retry of the same call and
has to carry the state the server issued for that question, which is used
once. For clients on older protocol versions, the SDK sends the elicitation
itself and re-invokes the handler. Both paths are tested.

A client that cannot elicit is refused, and the refusal names the command
line to run instead. `--confirm=approval` instead relies on the client's own
approval of the `apply_plan` call. It exists for clients without
elicitation, and is only as strong as the client's settings.

Plans live in memory, expire after `--plan-ttl` (ten minutes by default), and
are forgotten when the server stops, which is the safe direction.

### What plan_change offers

`drain` (with a mandatory reason) and `resume`. The agent writes the reason,
so it is held to the rules of `slurm node drain` before anything else happens:
no control characters or `|`, at most 200 characters, and quoted wherever the
user reads it. `resume` refuses a reason, since nothing would use it. The node
set has to be one Slurm reads as exactly those nodes, as for the command line,
so `ALL`, `NodeSet` names and nodes Slurm does not know are refused. Each entry in the `changes`
table in `internal/mcpserver/plan.go` names the gate verb, runs the action
through the same client the command line uses, gives the equivalent command
line, and says what to warn about. Adding power actions, boot overrides or
account changes means adding an entry. The gate, the pinning and the
confirmation come with it.

## Context, identity and state

- **One context.** The server resolves the configuration once at start and
  pins the context it names. A change to `currentContext` in a file cannot
  move a running agent to another cluster between two calls. Every result
  names the context.
- **Fresh configuration per call.** Each call builds its own `app.App`, so
  an edit to the inventory or to the protected hosts takes effect without a
  restart. Each call also gets its own streams, and nothing a command prints
  can reach the protocol on standard output. What a command reports goes to
  the call's `notes`; what a password helper writes on its standard error
  goes to the server's log instead, as the stack of a panic does.
- **No default node set.** `-n` and `CLUSTERCTL_NODES` do not apply, in the
  server's own tools and in the command tree `read_command` runs; a call
  names its nodes or selects nothing, as [safety.md](safety.md) requires.
- **No terminal.** Anything that would prompt refuses instead, including the
  BMC password prompt. ssh runs with `BatchMode yes` and, like a password
  helper, in a session of its own, so a host that falls back to password
  authentication fails the call rather than putting a prompt on the terminal
  the client runs in. A sops encrypted Secret is opened with the keys of
  `workstation.identities` alone: sops' own key discovery can run
  `SOPS_AGE_KEY_CMD` or have gpg-agent ask for a passphrase, so it is used
  only at a terminal. The `sops` command that decrypts is run only for an
  identity file that holds a recipient of the Secret, in a session of its
  own with no controlling terminal, with `HOME` and `XDG_CONFIG_HOME` pointed
  at an empty directory and an environment of `PATH`, `LANG`, `LC_ALL`,
  `LC_CTYPE`, `TMPDIR` and `TZ` alone, so neither `SOPS_AGE_KEY*` nor a key
  service's credentials reach it, and over pipes of its own rather than the
  protocol's streams ([ADR 0019](adr/0019-decrypt-with-the-sops-command.md)).
- **The administrator's identity.** It runs as the user who started it, with
  their ssh agent. A shared server reachable over HTTP would change whose
  keys act on the cluster, so that is a separate decision.
- **An audit trail.** Every plan, refusal and apply is appended to
  `$XDG_STATE_HOME/clusterctl/mcp/audit.jsonl`: time, context, plan, action,
  nodes, count, and outcome. An apply writes two lines, `applying` before
  anything is sent and then `applied` or `failed`. A plan or an apply that
  cannot be recorded is refused; a refusal that cannot be recorded says so.
- **Two calls at a time.** The SDK starts every tool call as soon as it
  arrives, and each call can reach as many hosts at once as `fanout.max`
  allows, so calls an agent sends side by side would multiply that. The
  server works on two at once; another call waits until one of them ends, and
  a call the client gives up on while it waits is not run. Every tool counts,
  since each reads the configuration afresh and may reach a host:
  `select_nodes` can run a group source's command. A place is taken for each
  run of a handler, not for the whole call. `apply_plan` ends one run when it
  puts its question, and the answer starts another, whether the client sends
  it back on a retry of the call or, for a client on an older protocol, the
  SDK asks and runs the handler again. A question left open holds no place,
  so two of them do not stop every other call; once answered, the apply may
  wait for a place like any other call before anything is sent.
- **A panic is a failed call.** A panic in a handler is recovered and
  reported as `failed:`, so it does not end the server and the plans waiting
  in it. A panic in the work for one node of a fan-out, which the handler's
  recover cannot reach, is that node's failure: the other nodes are
  reported, the command exits 1, and the stack goes to the server's log.

## Errors

A failed call starts with its kind, taken from the exit code: `rejected:`
(usage or configuration, 2), `unreachable:` (3), `not confirmed:` (130), or
`failed:` (1). The agent can then tell its own mistake from a host that did
not answer. `describe_nodes` reports a facet it could not read under
`errors` and returns the rest, because it is often called precisely when
something is down. It shows each node's service processor as the `bmc`
commands reach it, the inventory's `bmcAddress` first, and a node it cannot
name one for gets the reason under `bmcError` rather than an empty field.

## Size

Results are bounded so that a thousand nodes do not flood the agent's
context: `select_nodes` lists names up to 256, `describe_nodes` describes
64, `query_slurm` returns 200 rows by default and 2000 at most, and
`read_command` cuts output at 64 KiB. A command that prints more is stopped
at that point rather than buffered, so a jq program without end costs no more
than the bound. Every bound is reported (`truncated`), and `read_command`
accepts `-o jq=EXPR` to filter on the server. The program is compiled before
the command runs, runs with the call's context, so it stops when the call is
cancelled, and cannot read the server's environment through `$ENV` or `env`.
