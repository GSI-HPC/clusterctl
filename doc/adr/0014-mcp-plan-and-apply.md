<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# 0014 — An MCP server of curated tools, where a person answers the gate

Status: accepted

## Context

Administrators want to work with an AI agent in an MCP client such as Claude
Code, from the workstation where they already run clusterctl. The agent should
be able to find out what is going on and to propose changes.

The command line's safety model assumes a person at a terminal: a destructive
command previews and asks, and without a terminal it refuses unless `-y` was
given. An MCP server has no terminal. Whatever answers the gate under MCP
decides whether the safety model survives.

There are about 90 commands. Offering each as a tool is mechanical, but the
number makes an agent choose worse, and several commands carry different
risks behind one argument.

## Decision

`clusterctl mcp serve`, a stdio server in the same binary (`internal/mcpserver`
on the official Go SDK), offering:

- a few tools shaped around the common questions: `select_nodes`,
  `describe_nodes`, `query_slurm`;
- `read_command`, which runs any command the command tree marks read-only;
- `plan_change` and `apply_plan`, the only way to change anything.

`apply_plan` puts the gate's question to the administrator through an MCP
elicitation, and judges the answer by the gate's own rule. The agent cannot
answer it. A client that cannot elicit is refused unless the server was started
with `--confirm=approval`.

Running commands on the nodes, and the interactive commands, are not offered.

## Why

- **The gate keeps its meaning.** The question is the same one the prompt asks,
  including typing the host count above the threshold, and a person answers it.
  `-y` is never implied.
- **What is confirmed is what runs.** The plan pins the resolved node set and
  lists the exact commands, recorded by running the action against the dry-run
  recorder. It is applied once, and only after the gate is re-checked against
  the current configuration.
- **The long tail stays reachable without growing the tool list.**
  `read_command` covers every read-only command, and its description is built
  from the tree.
- **One table decides what is read-only.** `internal/cli/effects.go` is
  reviewed as a whole, and a test fails when it and the tree disagree.

## Costs

- Two surfaces to keep in step: the curated tools and the command line. The
  curated tools call the same clients (`app.Slurm`, `safety.Gate`), so they
  share behaviour, but a new question may deserve a tool before it gets one.
- Elicitation is required by default. A client without it can only read unless
  the administrator opts into `--confirm=approval`, which is only as strong as
  the client's approval settings.
- The first version offers only `drain` and `resume` as changes. Every other
  change is still done at the terminal.
- `exec` is out, and much triage on a node is a command away. The agent has to
  hand such a command to the administrator.

## Reconsider when

- A change is needed often enough from an agent: add it to the `changes` table.
- Running commands on the nodes is wanted: add it as a plan (the command, the
  pinned set, the gate) rather than as a direct tool, possibly with a configured
  allowlist of commands that only read.
- A shared server over HTTP is wanted: that changes whose identity acts on the
  cluster and needs its own record.
