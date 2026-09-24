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

3. **The action is previewed.** What will happen, to how many hosts, and which
   ones — before anything is sent.

4. **It is confirmed.** Below `safety.confirmAbove` hosts, a yes is enough.
   Above it, the host count has to be read off the preview and typed back,
   because a `y` is too easy to type by reflex:

   ```
   About to power off 40 hosts: exe[0001-0040]
   This is more than 8 hosts. Type the number of hosts to continue:
   ```

5. **Without a terminal, it does not run.** A destructive command in a script
   that cannot be asked is refused, not carried out. `-y` confirms in advance
   and is the supported way to automate.

`--dry-run` stops after the preview and exits zero, having sent nothing.

Steps 2 to 4 are also available separately. `Gate.Preview` runs the checks and
describes the question without asking it, and `Preview.Accept` judges an
answer by the same rule as the prompt. The MCP server uses these to put the
question to the administrator through the client instead of a terminal; see
[mcp.md](mcp.md).

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

**Power actions ask Slurm first.** A node running a job is refused, because
powering it off loses the job. `--force` overrides, and the check is skipped
when the workload manager cannot be reached — it is a safeguard, not a
dependency.

**A power-on is spread over batches.** `safety.powerOnBatch` nodes at a time
with `safety.powerOnStagger` between them, because a rack powering on at once
trips its breaker.

**A boot source override applies once by default.** A persistent override is
what leaves a machine reinstalling every time it reboots, so `--persistent` has
to be asked for. The same applies to a PXE boot path.

**Draining a node needs a reason.** The reason is the first argument, not an
option, because a drained node with no reason is one nobody dares resume.

**A Redfish action is never retried.** A reset that timed out may well have
been carried out; sending it again would power cycle a running machine. The
retry policies of general-purpose HTTP clients do exactly this, which is why
the client here has none.

**A reset type is checked before it is sent.** The machine is asked what it
accepts, so unsupported firmware is reported by name instead of rejecting an
opaque request. The shell tool sent `GracefullShutdown`, which no BMC accepts
and which nothing noticed.

**A reinstall resolves everything before it changes anything.** Boot paths and
addresses for the whole set are worked out first, so a set with one unknown
node stops before the first machine is touched rather than halfway through.

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
