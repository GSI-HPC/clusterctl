<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Safety

The shell toolkit had no preview, no confirmation and no dry run, and one of
its tools scheduled a reboot and mailed every logged-in user when it was asked
for its help text. Several tools fell back to a possibly stale node set when
none was given.

Everything that changes or destroys something goes through
`internal/safety.Gate` first.

## What a destructive command passes through

1. **A node set must have been selected.** There is no fallback to a set left
   over from an earlier command, and an empty selection is a usage error. An
   explicit `-n` is final: given empty, as `-n "$(...)"` is when the command
   inside selects nothing, it is refused and `CLUSTERCTL_NODES` is not read in
   its place. A node set given both with `-n` and as an argument, or with `-n`
   twice, is refused rather than one of them being dropped.

2. **Protected hosts are refused.** `safety.protectedHosts` is a list of node
   set expressions. A command touching one of them stops and names it; only
   `--force` gets past.

   ```
   $ clusterctl bmc power off -n exe[1-4],wlm01
   clusterctl: power off would touch the protected host wlm01; pass --force to do it anyway
   ```

   The comparison is by machine, not by spelling. `App.Select` maps every
   name to the one the inventory uses before the gate sees it: case and a
   final dot are dropped, other padding is resolved, and the host name and
   the service processor name the naming rules give a node, and the
   addresses the inventory records for it, all become the node's name. So
   `WLM01`, `wlm01.`, `wlm01.hpc.example.org`, `wlm01.mgmt.hpc.example.org`
   and `10.0.1.1` are all wlm01, and one machine named several ways is one
   target, reset once. The protected host entries are resolved the same way,
   so an entry may name a machine by any of these too. A name with a domain
   the rules do not give its short name is left as written, and counts as
   protected when its short name is.

3. **Names the inventory does not know are refused.** A name no machine of the
   inventory answers to cannot be told apart from another spelling of a
   protected host, so a change to it needs `--force` as well. A site without
   an inventory has nothing to compare with and is not checked. The Slurm
   accounting commands and `boot sync`, which name no node, are not checked
   either.

4. **The action is previewed.** What will happen, to how many hosts, and which
   ones — before anything is sent.

5. **It is confirmed.** Up to `safety.confirmAbove` hosts, a yes is enough.
   Above it, the host count has to be read off the preview and typed back,
   because a `y` is too easy to type by reflex. At `0` the count is typed
   for every action, and a negative value is refused:

   ```
   About to power off 40 hosts: exe[0001-0040]
   This is more than 8 hosts. Type the number of hosts to continue:
   ```

6. **Without a terminal, it does not run.** A destructive command in a script
   that cannot be asked is refused, not carried out. `-y` confirms in advance
   and is the supported way to automate.

`--dry-run` stops after the preview and sends no change. Read-only lookups
still run for real: the node set is resolved, group sources such as
`@slurm:main` are asked over ssh, the DHCP server's configuration is read, and
the checks a real run makes, such as the Slurm job check or a drain's reason,
are made the same way. The rehearsal therefore selects and addresses the same
hosts the real run would, refuses what the real run would refuse with the same
exit code, and exits zero only when the real run would go ahead. Only the
changes are recorded and printed instead of sent. A lookup that a dry run
skips is named in the preview, so a preview never reads a check it did not
make as passed.

Steps 2 to 5 are also available separately. `Gate.Preview` runs the checks and
describes the question without asking it, and `Preview.Accept` judges an
answer by the same rule as the prompt. The MCP server uses these to put the
question to the administrator through the client instead of a terminal; see
[mcp.md](mcp.md).

## Where the protected hosts come from

Each entry of `safety.protectedHosts` is resolved into machines the way a
selection is, group references included, and every machine it names has to be
one the inventory knows. An entry written as a host name or in capitals used to
protect only that spelling, and so, silently, nothing; now it protects the
machine, and an entry naming no machine the inventory knows is refused.

Entries without a group are resolved when the configuration is loaded, so a
wrong one stops every command, `config validate` included. Entries with a group
are resolved the first time a command is about to change something, so that a
group source that cannot be asked does not stop the commands that only look;
`config validate` resolves them too. An entry that cannot be resolved refuses
every change rather than protecting nothing. `--force`, which gets past the
protected hosts anyway, gets past that as well, and says so.

## What a command does

Every command records its effect, `read`, `change` or `interactive`, from one
table in `internal/cli/effects.go`. A test keeps the table and the command tree
in step. Anything that drives the tree for someone else reads the effect rather
than keeping its own list, and a command without one counts as a change.

## Checks specific to what is being done

**exec asks only when told to, but always checks.** Running a command is how
most of the day is spent, so exec asks nothing by default; `--confirm` adds the
question. The protected host check runs on every exec, so a plain run, a
confirmed run and a dry run make the same decision. With `--stdin` the payload
takes up standard input, so the question cannot be read from it and
`--stdin --confirm` needs `-y`. The command has to follow `--`, because a
word of it read as one of clusterctl's options, a `-n` or `-r`, would change
where or as whom it runs.

**Power actions ask Slurm first.** `bmc power off`, `soft`, `cycle` and
`reset`, and a `bmc redfish post` to a path that names a reset, ask `sinfo`
on the `slurm.role` host about every node in the set, because powering off a
running job loses it. The check fails closed. A node counts as idle only when
Slurm reports it in a state known to run no job, such as `idle`, `drained` or
`down`. These are refused:

- a node in a state that runs jobs, `allocated`, `mixed`, `completing`,
  `draining` or `failing`, whatever flags follow the state;
- a node in a state the check does not know;
- a node Slurm did not report, for example because its Slurm name differs
  from its inventory name;
- every node, when Slurm cannot be asked.

`--lose-jobs` overrides the check, and names the nodes it lets through. It is
separate from `--force` on purpose: getting past a protected host does not
also lose the jobs of the rest of the set. The protected-host check runs
first, so a refusal never leaves a protected host unnamed.

The check is on by default (`safety.slurmAware: true`) wherever `slurm.role`
names a host. When it is turned off, or no role is set, the command says so
before it asks for confirmation. A dry run asks Slurm too, so that it refuses
what the real run would. A set whose host list is too long for one argument
is not named to `sinfo`; every node is asked for and the answer narrowed to
the set.

**A power-on or a power cycle is spread over batches.** `safety.powerOnBatch`
nodes at a time with `safety.powerOnStagger` between them, because a rack
powering on at once trips its breaker, and a cycle powers it on again. A batch
with a failure stops the run: the failure may be the breaker. Every node of the
set is reported either way, the later batches as `not tried`, and the whole run
prints one result. A batch of less than one node is refused, in the
configuration and on the command line, rather than read as "no batches".

**An action falls back to another transport only when it never arrived.** A
read of the power state that fails over Redfish is tried over IPMI, when the
node's `bmc.order` names both. An action is tried again only when the request
provably never reached the service processor, because its name did not resolve
or nothing accepted the connection. A certificate that no longer matches its
pin is never a reason to try another transport.

**An interrupt leaves the unsent alone.** Once a BMC command is interrupted,
nothing more is sent. What was not sent reads `not sent`, and an action that
was under way reads `outcome unknown`, because it may have been carried out;
re-running for the failed nodes must not cycle those a second time.

**Forgetting a certificate pin is a change.** `bmc forget` shows the
fingerprints it is about to drop and goes through the confirmation like any
other change, protected hosts included. The pin is the only trust anchor for
a service processor's certificate: without it, the next connection trusts
whatever it is shown and sends it the BMC account.

**A boot source override applies once by default.** A persistent override is
what leaves a machine reinstalling every time it reboots, so `--persistent` has
to be asked for. The same applies to a PXE boot path: the persistent link, asked
for with `--persistent` or by a boot path rule marked `static`, is the one named
with `services.pxesrv.staticSuffix`, and without a suffix it is refused rather
than written as the one-shot link. `boot unset` removes both links, `boot
status` shows both, and a one-shot path is refused for a node that has a
persistent one. A GRUB link over TFTP has no one-shot form, so its preview says
that it stays until `boot grub unset`.

**A boot link is checked before it is written.** The address a link is named
after has to be an IP address that no other node has, and every boot path has
to exist on the PXE host, all before the question, which lists each boot path
with the nodes and addresses it is written for. A dry run sends nothing to the
PXE host and says what it left unchecked. Every node is tried and reported, so
a link that cannot be written neither stops the rest nor goes unnoticed.

**Draining a node needs a reason.** The reason is the first argument, not an
option, because a drained node with no reason is one nobody dares resume. It is checked before the preview: no control characters, which could
rewrite what the confirmation shows, no `|`, which Slurm's parsable output
uses between fields, at most 200 characters, and nothing that reads as nodes,
which is what a forgotten reason looks like. The preview quotes it.

**Slurm has to read a node set as itself.** `scontrol update` expands `ALL`
to every node and a `NodeSet` name to its members, which the gate would count
as one host. Before a drain or a resume is previewed, `sinfo` is asked for the
set and has to return exactly the nodes named; `ALL` is refused outright. The
check reads the cluster under `--dry-run` too.

**A Redfish action is never retried.** A reset that timed out may well have
been carried out; sending it again would power cycle a running machine. The
retry policies of general-purpose HTTP clients do exactly this, which is why
the client here has none.

**A reset type is checked before it is sent.** The machine is asked what it
accepts, so unsupported firmware is reported by name instead of rejecting an
opaque request. The shell tool sent `GracefullShutdown`, which no BMC accepts
and which nothing noticed.

**A reinstall resolves everything before it changes anything.** For the whole
set, the address, boot path, service processor and BMC credential of every
node are worked out first, the DHCP configuration read once, the boot paths
checked on the PXE host and Slurm asked, all before the question, which lists
each boot path with its nodes. A set with one node that cannot be reinstalled
stops before the first machine is touched rather than halfway through. The
boot override is set over Redfish, so a node whose `bmc.order` starts with
another transport is refused. The host keys, which cannot be put back, are
forgotten only once every machine is armed, just before the reset.

**A failed reinstall disarms what it armed.** When a step fails, the boot
override and the boot link of every node that was not reset are removed again,
and the error names what is reinstalling, what was disarmed and what could not
be, with the `bmc boot unset` and `boot unset` commands that disarm it. The
table lists every node with what became of each step. `--no-reset` leaves the
set armed on purpose and says so.

**A boot address comes from the node's own DHCP declaration.** An address not
in the inventory is taken only from the declaration named after the node or
its fully qualified name. Another interface, the BMC, or a neighbour whose
comment names the node never supplies it, and two candidates are refused
rather than one picked, so a reinstall cannot arm a machine nobody selected.

## Exit codes

Scripts branch on these, so they may be added to but never renumbered.

| Code | Meaning |
| --- | --- |
| 0 | Everything succeeded |
| 1 | clusterctl worked, but at least one target failed |
| 2 | The command line or the configuration was rejected |
| 3 | A host could not be reached or authenticated with |
| 130 | Interrupted, or a confirmation was declined |

The distinction between 1 and 3 is what lets a wrapper tell "the node said no"
from "the node was not there".
