<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Selecting nodes

Every command that takes `-n`, and every MCP tool that takes a node set, turns
an expression into nodes the same way. The expression is written in the node
set language, which [nodeset.md](nodeset.md) describes, and parsed by the
`nodeset` package. This document covers what clusterctl adds around it: the
sources a `@group` is resolved from, and how `App.Select` turns the names an
expression resolves to into the machines of the inventory.

## Groups

A `@group` reference is resolved by a source. Three kinds exist:

```yaml
groups:
  defaultSource: inventory
  sources:
    inventory:
      # One group per value of a node attribute: @exe, @wlm, @dbm.
      # This is what the genders file provided.
      attribute: class
    static:
      # A table written in the configuration. A group may name others.
      static:
        infra: wlm01,dbm01
        compute: "@inventory:exe"
    slurm:
      # Asked of the workload manager, cached for a minute.
      cacheTtl: 60s
      exec:
        role: login
        map: [sinfo, -h, -o, "%N", -p, $GROUP]
        all: [sinfo, -h, -o, "%N"]
        list: [sinfo, -h, -o, "%R"]
        reverse: [sinfo, -h, -N, -o, "%R", -n, $NODE]
```

A bare `@group` searches the default source first and then the others, so a
single-source installation never needs a prefix. `@source:group` names one.

The search moves on only when a source answers that it has no such group: a
table or an attribute that does not have it, or an `exec` source whose `list`
command succeeds and does not name it. A source that cannot be asked, whose
command fails, or that returns no nodes for a group it lists stops the search
with its own error, and the command fails with that error's exit code, 3 for
a host that could not be reached. Another source's group of the same name is
never used in its place. An `exec` source without a `list` command cannot say
that a group is not its own, so a bare name that reaches it stops there; name
the source of a group that lives further down the search order.

An `exec` source runs on the host role it names, which is required, and each
command is bounded by `fanout.commandTimeout` on that host. A cached answer is
stored under a key made of the site, the cluster and the context, the host
the command ran on and the exact argument vector, so no two clusters, and no
two group names, ever share an entry.

Within one command each lookup is made once. Callers that ask for the same
group, listing or `@source:*` at the same time wait for one command and share
its answer, a failure included. A group's nodes, and that a source has no
such group, are then kept for the rest of the command, whatever `cacheTtl`
says; a source that could not be asked, or a lookup an interrupt stopped, is
not, and the next caller asks again. Whether a group is not an `exec`
source's is decided on a listing made during the command, never on one from
the cache, since that may predate the group: it is made once, however many
groups the command finds missing.

A source that fails does not hide what the others found: `node groups` and
`node describe` print the memberships and groups that could be read, name the
failed source on the error stream and exit non-zero.

An `exec` source is given an **argument vector**, not a command line, and
`$GROUP` and `$NODE` are substituted as whole arguments. A group name holding a
semicolon stays a group name.

Groups may refer to groups, and a cycle is reported rather than looping;
[nodeset.md](nodeset.md#groups) says how deep.

`@source:*` of a source without an `all` command is the union of its groups,
each evaluated on its own as `@a,@b` evaluates them. The resolver hands the
parser one expression, which is read left to right, so a group whose value
holds `!`, `&` or `^` goes into it as the reference `@source:group` rather
than as its value, where the operator would apply to every group before it.
A name that would not read back as that one reference, one holding a space,
a comma, an operator or a bracket, is refused.

## Names are host names

The node set language accepts more than a host name may contain, because a
set is also used for things that are not hosts; of the host name rules, the
parser itself enforces only that a name does not begin with `-`. A node name, though, becomes
an ssh destination and the host of a Redfish URL, where a leading `-` is an
option and `:`, `@`, `/`, `?` and `#` set the port, the account, the path, the
query and the fragment. So `App.Select`, which every command and the MCP
server use to turn an expression into nodes, refuses a selection with any name
that is not a host name: dot separated labels of ASCII letters, digits and
hyphens, none empty, none longer than 63 characters, none beginning or ending
with a hyphen, and at most 253 characters in all, with an optional final dot.
The check runs on every name the expression resolves to, so names from a group
source or the inventory are held to it too. It lives in `internal/hostname`,
and ssh and the Redfish client apply it again to the host they are given.

## Names are machines

`App.Select` then replaces each name with the name the inventory uses for the
machine it refers to (`App.canonicalize`). Case and one final dot are dropped;
a name written with other padding is resolved; and the host name and the
service processor name the naming rules give an inventory node, and its
`address` and `bmcAddress`, are that node. A dotted name the inventory does not
list is reduced to its short name when it is the host name or service processor
name the rules give that short name. Anything else is kept, lowercased.
Because the result is a set, one machine named several ways is one member. An
alias two inventory nodes share names neither, and an inventory name is never
taken over by another node's alias. Nor is any other spelling of it: a name
that is one node's written with other padding or as its host name, and another
node's alias as well, could be either machine, and is refused as ambiguous with
a usage error. With `bmcAddress: exe4` on `exe0003`, `exe4` would otherwise
reach `exe0003` while `EXE04` reached `exe0004`; `exe0004` itself stays
`exe0004`. The one node that `login`, `node describe` and `node groups` take as
an argument is resolved the same way (`oneNode`), so that `login exe1` reaches
the `exe0001` that `exec -n exe1` does, and `node describe` finds a node by its
host name.

## One machine, one spelling

In the node set language padding is not part of a host's identity
([nodeset.md](nodeset.md#padding-is-not-part-of-a-hosts-identity)): `exe1` and
`exe0001` are one host. This is what lets an administrator type `exe1` and
reach the machine an inventory wrote as `exe0001`, and see it under the name
the site gave it, because a selection is canonicalised against the inventory.

The price is that a site cannot have two machines whose names differ only in
padding: they would be one host to every command, so an inventory holding such
a pair has to be rejected rather than one of them picked. A node set does not
report such a pair, so the inventory checks its names itself as it reads
them. It refuses an entry that names a host with other padding, or other case,
than the entry that first named it: `exe1` after `exe[0001-0010]` could be a
refinement of `exe0001` or a second machine, and which one was meant cannot be
told. It refuses capitals outright: every lookup of a node lowercases the name
it is given, and the node set does not fold case, so `EXE0001` would never be
found and would lose its `bmcAddress` to the naming rules.
