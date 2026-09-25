<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# CLI review: what each command is for, and where the surface could go

A review of the command tree as of `v0.2.0` plus `main` at `979f89d`. It asks
four questions of every command: how useful it is to an HPC site that is not
GSI, who uses it, what for, and at what level of abstraction it works. Then it
proposes changes to the surface, each with the reason for it.

The tree was taken from the built binary (`--help` of every command). The
implementation was read wherever it decides portability: hard-coded tools,
paths and naming conventions, and configuration that is read or not. The
local commands were run against `examples/site/`, and the read commands were
run with a stub `ssh` on `PATH`.

This is a proposal, not a record of a decision. Nothing here changes the
program.

## Summary

- **93 leaf commands under 22 top-level nouns**: 56 read, 28 change,
  9 interactive (from `internal/cli/effects.go`), plus `completion`.
- **The binary holds two products.** About two thirds of the surface (62
  leaves: `login`, `exec`, `copy`, `node`, `hostkey`, `tunnel`, `dns`, `bmc`,
  `slurm`, `secrets`, `config`, `doctor`, `mcp`) works at any Linux, Slurm
  and Redfish site with configuration alone. The other third (31 leaves:
  `boot`, `dhcp`, `cinc`, `provision`, `pdu`, `fabric`, `hca`) encodes one
  toolchain: pxesrv links, ISC dhcpd, DHCP over IPoIB, cinc-solo recipe
  URLs, NVIDIA MFT and a rack naming convention.
- **The core is strong and has no real competitor as one tool.** It has a
  safety gate, Slurm-aware power, Redfish with certificate pinning and no
  retries, exact argv quoting, layered configuration with provenance, and
  structured output. These are the parts other sites would adopt.
- **The grammar has drifted.** The same short flag means different things,
  a node set comes before or after its operand depending on the command,
  "boot" names three different mechanisms, and two commands read or write
  depending on how many arguments they get.
- **Global flags are accepted everywhere and silently ignored where they do
  not apply.** One of them, `--dry-run`, makes several read commands print
  fabricated results (see [F7](#f7-global-flags-apply-everywhere-and---dry-run-changes-what-reads-report)).
  That one is a bug, not a matter of taste.
- **Six configuration sections are accepted and read by nothing.** The
  project's own rule forbids that (requirements.md, R21).
- **The agent sees more than the administrator.** The MCP `describe_nodes`
  tool combines inventory, Slurm state and jobs. The CLI has no command that
  does.
- **The window for breaking changes is now.** The CLI is at `v0.x`, the
  schema at `v1alpha1`, and nothing outside GSI depends on it yet.

## The scales

**Generic usefulness** to a site that is not GSI:

| Rating | Meaning |
| --- | --- |
| High | Works at any Linux HPC site (with Slurm, where it touches Slurm) from configuration alone; no GSI convention in the code |
| Medium | Generic idea, but the implementation assumes one tool or vendor that many sites share (InfiniBand, NVIDIA MFT, ISC dhcpd, fping), or carries a GSI convention in a default |
| Low | Useful only to a site that adopts GSI's own toolchain (pxesrv, the cinc-solo archive model, the PDU naming) |

**Audience**:

| Short | Who |
| --- | --- |
| Ops | The day-to-day or on-call cluster administrator |
| HW | Hardware and data centre work: service processors, racks, cabling |
| Prov | Provisioning and OS images |
| Fabric | The interconnect |
| Sched | Slurm administration and user support |
| Integrator | Whoever maintains the site configuration, onboards clusterctl and runs it in CI |
| Sec | Whoever owns the trust anchors: host keys, certificate pins, secrets |
| Remote | An administrator outside the site network |
| Agent | An AI agent through `mcp serve` (read commands only) |
| Script | Automation that consumes `-o json` and exit codes |

**Level of abstraction**:

| Level | Meaning | Example |
| --- | --- | --- |
| L0 escape hatch | Raw access to a host or an API, with clusterctl's transport and quoting | `login`, `exec`, `bmc redfish get` |
| L1 model query | Answered from configuration and inventory, nothing contacted | `node select`, `pdu list` |
| L2 primitive | One typed read or change on one subsystem | `bmc status`, `boot set`, `hca config get` |
| L3 guarded operation | A primitive plus checks against another subsystem, batching or reconciliation | `bmc power`, `slurm node drain`, `secrets push` |
| L4 workflow | Several subsystems in sequence, with rollback | `provision reinstall` |
| Meta | About the tool itself | `config`, `doctor`, `mcp`, `version` |

## Overview

| Command | Leaves R/C/I | Generic | Audience | Level | Verdict |
| --- | --- | --- | --- | --- | --- |
| `login` | 0/0/1 | High | everyone | L0 | Keep; absorb the four `shell` commands |
| `exec` | 0/1/0 | High | Ops, Script | L0 | Keep; this is the `clush` replacement |
| `copy` | 0/1/0 | High | Ops | L0 | Keep; split push from pull |
| `node` | 8/0/0 | High (`hw` Medium) | everyone, Integrator | L1 | Keep; add live status and an audit |
| `slurm` | 9/7/0 | High at Slurm sites | Sched, Ops | L2–L3 | Keep; fix grammar, fill maintenance gaps |
| `bmc` | 5/5/1 | High | HW, Ops | L0–L3 | Keep; the strongest generic asset, grow it |
| `pdu` | 1/0/1 | Low | HW | L0–L1 | Rework around outlets, or drop from the core |
| `fabric` | 3/0/0 | Medium | Fabric | L2–L3 | Merge with `hca` |
| `hca` | 4/1/0 | Medium | Fabric, HW | L2 | Merge into `fabric` |
| `boot` | 4/5/1 | Low (`grub` Medium) | Prov | L2–L3 | Rename; put a driver seam under it |
| `dhcp` | 3/0/2 | Medium–Low | Prov | L0–L2 | Keep as ISC diagnostics behind a driver seam |
| `cinc` | 1/2/1 | Low | Prov | L2 | Generalise into configuration management with drivers |
| `secrets` | 2/1/0 | High | Sec, Prov | L3 | Keep; decouple from cinc, target per node |
| `provision` | 1/1/0 | Low–Medium | Prov, Ops | L4 | Keep; add the second half of a reinstall |
| `hostkey` | 3/2/0 | High | Sec, Ops | L2–L3 | Keep |
| `tunnel` | 2/1/1 | Medium | Remote | L2 | Keep |
| `dns` | 2/0/0 | Medium | Ops, Integrator | L2 | Keep; feed it into an audit |
| `config` | 6/1/0 | High | Integrator | Meta | Keep |
| `doctor` | 1/0/0 | High | Integrator, Ops | Meta | Keep |
| `mcp` | 0/0/1 | High | Agent | Meta | Keep |
| `version`, `completion` | | High | everyone | Meta | Keep |

## Per command

### Everyday: login, exec, copy, node

**`login [ROLE|HOST|NODE] [-- COMMAND]`**: interactive, High, L0.
Audience: everyone. Use cases: open a shell on an infrastructure role with
that role's account, jump host and agent forwarding; run one command there
with the argument vector intact; reach a node by its short name; with
`--dry-run`, print the exact ssh line for debugging. The role names come
from configuration; the only built-in names are `login` and `mgmt`, as the
default target. `boot shell`, `dhcp shell` and `cinc shell` are
`login <services.X.role>` under another name, and `pdu shell` is `login` to a
derived host ([F8](#f8-service-shells-duplicate-login)).

**`exec [NODESET] -- COMMAND`**: change (asks only with `--confirm`), High,
L0 with guard rails. Audience: Ops, Script. Use cases: ad-hoc checks, version
surveys with `--dedup`, fix-up scripts with `--script`, a payload on
`--stdin`. It refuses protected hosts, quotes the argument vector exactly,
enforces the timeout on the node and escapes control characters. It does not
ask Slurm about running jobs, by design and documented. Its `-b` collides
with `--bmc` elsewhere ([F6](#f6-short-flags-collide-against-the-trees-own-rule)).

**`copy [-n NODESET] SOURCE... DEST`**: change (always asks), High, L0.
Audience: Ops. Use cases: push a configuration file to a node set; with
`--download`, collect logs into one directory per node. The download
direction asks too, although it only writes locally. `exec`, which can run
anything as root, does not ask by default. The confirmation is inverted
relative to the risk ([F13](#f13-confirmation-is-inverted-between-exec-and-copy)).

**`node`**: all read, High, mostly L1. Audience: everyone; Integrator for
the inventory, HW for racks. Replaces `nodeset`, `nodeattr`/genders and the
rack spreadsheet.

| Leaf | Generic | Level | Notes |
| --- | --- | --- | --- |
| `select [EXPR]` | High | L1 | The node set calculator; the same engine as the public `nodeset` package |
| `list [NODESET]` | High | L1 | Inventory table |
| `describe NODE` | High | L1 | Inventory, names and groups of one node. No live state, while MCP `describe_nodes` has Slurm state, reason and jobs ([F11](#f11-the-agent-sees-more-than-the-administrator)) |
| `attrs [KEY]` | High | L1 | The genders `nodeattr` answer. Accepts and ignores `-n` |
| `groups [NODE]` | High | L1–L2 | Asks exec group sources remotely |
| `rack [RACK\|NODE]` | High | L1 | |
| `fqdn [NODESET]` | High | L1 | Naming rules; `--bmc` for service processor names |
| `hw [NODESET]` | Medium | L2 | DMI from sysfs is generic. The InfiniBand column greps `lspci` for Mellanox part numbers (`MT[0-9]*`, `ConnectX`) |

### Scheduler: slurm

High at any Slurm site, which is most HPC sites. Audience: Sched, Ops, user
support. The value over raw `sinfo`/`scontrol`/`sacctmgr` is: parsable output
with an explicit field list, node set integration, the reason policy for
drains, name checks against Slurm's own expansion of `ALL` and NodeSets, and
the confirmation gate.

| Leaf | Effect | Generic | Level | Notes |
| --- | --- | --- | --- | --- |
| `node list [NODESET]` | R | High | L2 | State groups: alloc, defect, down, drain, idle |
| `node nodeset [STATE]` | R | High | L2 | Exists to feed `-n "$(…)"` ([F12](#f12-selection-goes-through-subshells)) |
| `node drain REASON [NODESET]` | C | High | L3 | Mandatory reason of at most 200 characters, without `\|` and without node names |
| `node resume [NODESET]` | C | High | L3 | |
| `job list [NODESET]` | R | High | L2 | `-u` is the job owner here and the remote account elsewhere ([F6](#f6-short-flags-collide-against-the-trees-own-rule)) |
| `job history` | R | High | L2 | Accounting window, states, job ids |
| `job summary` | R | High | L2 | Jobs per user and account; the user-support view |
| `partition [NAME]` | R | High | L2 | A leaf where its siblings are groups |
| `account list`, `limits` | R | High | L2 | |
| `account add ACCOUNT [ORG] [DESC]` | C | Medium | L2 | Optional operands by position ([F4](#f4-the-positional-grammar-is-not-uniform)) |
| `account coordinator ACCOUNT USER...` | C | Medium | L2 | Adds only; no removal |
| `account shares [ACCOUNT] [VALUE]` | C | Medium | L2 | Reads or writes depending on the number of arguments ([F5](#f5-some-commands-read-or-write-depending-on-their-arguments)) |
| `user list` | R | High | L2 | |
| `user add USER [ACCOUNT] [DEFAULT]` | C | Medium | L3 | Checks with `getent` and plans before writing |
| `user default USER ACCOUNT` | C | Medium | L2 | |

Account administration is Medium rather than High because many sites drive
`sacctmgr` from an identity or allocation system (ColdFront, Waldur, their
own sync) rather than by hand. It is still what a small or mid-sized site
wants.

Missing for everyday operations: `scontrol reboot` (reboot when idle and come
back in a given state), reservations for maintenance windows, removal of
users, coordinators and accounts, setting limits (they can only be read),
node features and GRES, and controller health (`scontrol ping`, `sdiag`).

### Hardware: bmc, pdu, fabric, hca

**`bmc`**: High, L0 to L3. Audience: HW, Ops. This is the most portable and
most carefully engineered part of the tree: Redfish first with IPMI
fallback, certificate pinning, actions never retried, the Slurm job check
before power, power-on batching against breaker trips, and the password
never in a remote argument vector.

| Leaf | Effect | Generic | Level | Notes |
| --- | --- | --- | --- | --- |
| `status [NODESET]` | R | High | L2 | Power state; falls back to IPMI for a read |
| `power ACTION [NODESET]` | C | High | L3 | Six actions behind one enum. `status` duplicates `bmc status` and makes the command mixed-effect ([F5](#f5-some-commands-read-or-write-depending-on-their-arguments)) |
| `boot show\|set\|unset` | R/C/C | High | L2 | Redfish boot override only; hardware that speaks only IPMI cannot be set, and so cannot be reinstalled |
| `redfish get PATH` | R | High | L0 | |
| `redfish post PATH BODY` | C | High | L0 | Gated; a reset path asks Slurm first |
| `redfish info` | R | High | L2 | Identity, health, the reset types the firmware accepts |
| `ping` | R | Medium | L2 | `fping` on the `bmc.ipmi.via` host |
| `forget` | C | High | L2 | Lifecycle of the certificate pins (Sec) |
| `web NODE` | I | High | L1 | URL, opened in the configured browser |

Portability: `bmc.redfish.systemPath` defaults to `/redfish/v1/Systems/1`.
That is right for iLO, XCC and Supermicro, but Dell iDRAC uses
`System.Embedded.1`. A vendor profile covers it; reading the `Systems`
collection when the path is unset would make the setting unnecessary.

Missing, and among the most frequent out-of-band tasks at any site: the
event log (SEL, IML), sensors and health, the serial console, firmware
inventory, and BIOS attributes ([C4](#c4-widen-bmc-to-what-hardware-triage-needs)).

**`pdu`**: Low, L0 and L1. Audience: HW.

| Leaf | Effect | Generic | Level | Notes |
| --- | --- | --- | --- | --- |
| `list` | R | Low | L1 | The row is the first digit of the rack name, hard-coded in `rowOf`, so `R01` gives `pdu0-R01`. Accepts and ignores `-n` |
| `shell ROW RACK` | I | Low | L0 | An ssh session to the PDU's own CLI. Here the row is given by hand; `list` derives it |

There is no outlet control and no mapping from a node to an outlet. The name
is derived by a convention written in code. Outside GSI there is little here
([C9](#c9-pdu-around-outlets-or-not-at-all)).

**`fabric`**: Medium, L2 and L3. Audience: Fabric. The idea is good:
derive the port GUID from the MAC that DHCP or the inventory knows, and ask
the subnet manager, so that a link can be checked before the OS is up.

| Leaf | Effect | Generic | Level | Notes |
| --- | --- | --- | --- | --- |
| `state [NODESET]` | R | Medium | L3 | Port state from the subnet manager, `Active` or not |
| `guid [NODESET]` | R | Medium | L2 | The Mellanox MAC-to-GUID rule is hard-coded; `services.fabric.guidFormat` is accepted but not read |
| `counters NODE` | R | Medium | L2 | One node only; `--uplink` reads the switch port |

It needs infiniband-diags on the fabric host. It offers nothing for
Slingshot, Omni-Path or RoCE.

**`hca`**: Medium, L2. Audience: Fabric, HW. The same port as `fabric`,
seen from the node instead of from the subnet manager, through NVIDIA MFT
(`mst`, `mlxconfig`, `mlxcables`) and `ibstat`. It is generic wherever there
are NVIDIA adapters, which is nearly every InfiniBand site.

| Leaf | Effect | Level | Notes |
| --- | --- | --- | --- |
| `link` | R | L2 | `ibstat` |
| `cable` | R | L2 | `mst cable add`, `mlxcables` |
| `firmware` | R | L2 | Overlaps `node hw` |
| `config get KEY` | R | L2 | `mlxconfig query` |
| `config set KEY VALUE` | C | L2 | Gated; checks key and value shapes |

### Provisioning: boot, dhcp, cinc, secrets, provision

This is where the GSI toolchain lives. The ideas are generic: point a node
at an image, boot it once from the network, give it its secrets, run
configuration management. The commands implement one specific way of doing
each of these.

**`boot`**: Low, L2. Audience: Prov. The contract is pxesrv's: a symbolic
link named after the node's IP address in the pxesrv root, with a static
suffix for a persistent link. Other sites use PXELINUX `01-<mac>` files, GRUB
per-MAC or per-IP files, iPXE chained over HTTP, or a provisioning system
with its own API (Warewulf, xCAT, Cobbler, Foreman, MAAS, OpenCHAMI BSS).

| Leaf | Effect | Generic | Level | Notes |
| --- | --- | --- | --- | --- |
| `set [NODESET] [PATH]` | C | Low | L3 | Cluster `bootPaths` rules, two matches is an error. PATH is recognised by a leading `/` ([F4](#f4-the-positional-grammar-is-not-uniform)) |
| `unset`, `status` | C, R | Low | L2 | |
| `list` | R | Low–Medium | L2 | `find` for `ipxe.*` and `grub.cfg*` |
| `log` | R | Low | L2 | Tail of one file |
| `sync` | C | Low | L2 | `git pull` in `repoPath` on the PXE host |
| `shell` | I | — | L0 | `login` to the pxesrv role |
| `grub set NODE TARGET` | C | Medium–Low | L2 | GRUB's hex-IP file name is a standard convention. One node, IPv4 only, persistent only, explicit target only (the `bootPaths` rules are not used) |
| `grub show NODE` | R | Medium | L1 | Computed locally |
| `grub unset NODE` | C | Medium–Low | L2 | |

**`dhcp`**: Medium to Low, L0 to L2. Audience: Prov. The parser handles ISC
dhcpd only, carefully: it follows includes and refuses what it does not
understand. ISC stopped maintaining ISC DHCP in 2022 and points to Kea; other
sites run dnsmasq, Infoblox, or a dhcpd that their provisioning system writes.

| Leaf | Effect | Generic | Level | Notes |
| --- | --- | --- | --- | --- |
| `hosts [NODESET]` | R | Medium–Low | L2 | Name, interface and comment matches |
| `config` | R | Medium–Low | L2 | Every host declaration |
| `log` | R | Low–Medium | L2 | Greps a syslog file, default `/var/log/syslog` (Debian's path, not RHEL's, and not journald) |
| `capture` | I | Medium | L0 | Bounded `tcpdump`; default interface `ib0` (DHCP over IPoIB, a GSI trait) |
| `shell` | I | — | L0 | `login` to the dhcp role |

**`cinc`**: Low, L2. Audience: Prov. A file at `/etc/cinc/solo` holds shell
assignments (the URL and `CHEF_RUN_LIST`), and
`cinc-solo --minimal-ohai --recipe-url URL [--override-runlist]` runs from
it. That is the chef-solo archive mode as GSI uses it. Other sites run
`ansible-pull`, `puppet agent -t`, `salt-call state.apply` or `cf-agent`.
`services.cinc.archivePath`, `baseCookbook` and `rolesPath` are in the schema
but nothing reads them; they point at an archive build step that was never
ported.

| Leaf | Effect | Level | Notes |
| --- | --- | --- | --- |
| `config URL [NODESET]` | C | L2 | `-R` is `--run-list` here and `--recursive` in `copy` |
| `run [NODESET]` | C | L2 | Reads the file, never sources it |
| `show [NODESET]` | R | L2 | |
| `shell` | I | L0 | `login` to the `services.http` role |

**`secrets`**: High, L1 to L3. Audience: Sec, Prov. Decrypt sops or age into
memory, stream it over stdin, write it atomically with owner and mode, and
check that every reference resolves without holding a key. Any site needs
this (munge keys, keytabs, certificates), and few tools get it right.

| Leaf | Effect | Level | Notes |
| --- | --- | --- | --- |
| `list` | R | L1 | Accepts and ignores `-n` |
| `check [--decrypt]` | R | L2 | |
| `push [NODESET]` | C | L3 | Decrypts everything before asking; an unreachable node is not retried |

Gaps: every secret goes to every node selected. There is no node set per
secret (the slurmdbd password belongs on the database host only) and no
source per node (host keytabs and host certificates differ per machine). The
list lives under `services.cinc.secrets`, although it has nothing to do with
cinc.

**`provision`**: Low to Medium, L4. Audience: Prov, Ops.

| Leaf | Effect | Generic | Level | Notes |
| --- | --- | --- | --- | --- |
| `reinstall` | C | Low–Medium | L4 | Resolves everything first and rolls back links and overrides on failure; that pattern is generic. The steps are pxesrv links plus the Redfish override |
| `status` | R | Medium | L4 | PXE link, power state and ssh reachability per node; really a node lifecycle view |

`reinstall` ends at the reset. The second half of a reinstall is left to the
administrator: wait for ssh, `hostkey refresh`, `secrets push`, `cinc run`,
`slurm node resume`. The old toolkit had `cluster-post-install` for part of
it ([C5](#c5-provision-finish)).

### Site access and trust: hostkey, tunnel, dns

**`hostkey`**: High, L1 to L3. Audience: Sec, Ops. One known-hosts file
under version control, rewritten under a lock. That is a generic good
practice most sites do not manage to keep.

| Leaf | Effect | Level | Notes |
| --- | --- | --- | --- |
| `list` | R | L1 | No filter |
| `scan`, `verify` | R | L2 | Handshake only; `verify` exits non-zero on a change |
| `refresh` | C | L3 | Lock and full rewrite |
| `remove` | C | L2 | |

`-b/--bmc` on all four is the flag that collides with `exec -b`.

**`tunnel`**: Medium, L1 and L2. Audience: Remote. sshuttle profiles built
from named networks, connecting through the generated ssh configuration. It
is generic for sites whose management networks are reachable only through a
gateway. Sites with a VPN, or that forbid sshuttle, will not use it; the
`proxyJump` of a role covers the ssh part without it.

**`dns`**: Medium, L2. Audience: Ops, Integrator. `lookup` asks the name
server directly and reports every answer, which is a good generic
diagnostic. `aliases` answers one narrow question: which machines are in the
login pool.

### Tool: config, doctor, mcp, version, completion

**`config`** (High, Meta, Integrator): `init`, `validate`, `view
--show-sources`, `explain` and `schema` are exactly what an adopting site
needs. `use-context` prints the line to change instead of changing it, which
surprises anyone who knows the kubectl command the name comes from.
`CLUSTERCTL_CONTEXT` exists for a single shell.

**`doctor`** (High, Meta): local checks, and with `--remote`, a check for
the tools on each role.

**`mcp serve`** (High, Meta): plan and apply, with the person answering the
gate. It is new, and portable as it stands.

## Findings that cut across commands

### F1. Two products in one binary

The portable core and the GSI provisioning chain share one namespace and one
help page. A visitor from another site sees `cinc`, `dhcp`, `hca` and `pdu`
next to `exec` and `bmc`, and cannot tell which ones they could use. The
chain's commands are written against one implementation each, with no seam
where a second one could be added.

### F2. Product names at the top level

`cinc`, `dhcp`, `hca`, `pdu`, and in effect `boot` (pxesrv), name tools, not
capabilities. That is fine for `slurm`, which is the de facto standard. For
the others it means a site running Ansible, Kea or Warewulf finds nothing it
can use, and a second implementation would need a second top-level noun.

### F3. "boot" means three things

| Command | Acts on | Grammar |
| --- | --- | --- |
| `boot set` | the pxesrv link on the PXE host | `[NODESET] [PATH]` |
| `boot grub set` | a GRUB link on the TFTP host | `NODE TARGET` |
| `bmc boot set` | the BMC's next-boot device | `TARGET [NODESET]` |

Two of them end in `boot set`. `provision reinstall` needs the first and the
third together, which is the only reason they are both needed.

### F4. The positional grammar is not uniform

- The operand comes first, then the node set: `bmc power ACTION [NODESET]`,
  `bmc boot set TARGET [NODESET]`, `cinc config URL [NODESET]`,
  `hca config set KEY VALUE [NODESET]`, `slurm node drain REASON [NODESET]`.
- The node set comes first and the operand is guessed from its shape:
  `boot set [NODESET] [PATH]`, where PATH is whatever starts with `/`.
- One node only, no `-n`: `boot grub set NODE TARGET`,
  `fabric counters NODE`, `node describe NODE`.
- Two positionals for one identity: `pdu shell ROW RACK`.
- Chains of optional positionals: `slurm account add ACCOUNT [ORG] [DESC]`,
  `slurm user add USER [ACCOUNT] [DEFAULT_ACCOUNT]`.

### F5. Some commands read or write depending on their arguments

`bmc power status` only reads, but it lives inside a change command.
`slurm account shares ACCOUNT` reads, while `slurm account shares ACCOUNT
VALUE` writes. The effects table has to mark both as changes. So
`read_command` cannot reach them through MCP, and a reader cannot tell from
the command name whether a script changes anything. `doc/mcp.md` already
names the `bmc power` enum as a problem ("six different risks behind one
enum").

### F6. Short flags collide, against the tree's own rule

`internal/cli/root.go` says that "a short flag means the same thing in every
command". It does not:

| Flag | Meanings |
| --- | --- |
| `-b` | `--bmc` in `dns lookup`, `hostkey refresh\|remove\|scan\|verify`, `node fqdn`; `--dedup` in `exec` |
| `-R` | `--recursive` in `copy`; `--run-list` in `cinc config\|run` |
| `-u` | the remote account in `exec`, `copy`, `login`; the job owner filter in `slurm job list\|history` |

`exec -b` is `clush -b`, which is muscle memory worth keeping. The other side
of each collision is the one to change.

### F7. Global flags apply everywhere, and `--dry-run` changes what reads report

`-n`, `--dry-run`, `-y`, `--force` and `--fanout` are persistent flags on the
root. Commands that do not use them accept them silently.
`clusterctl pdu list -n exe0001` lists every rack, and `secrets list -n
exe[1-2]` lists every secret, both with exit 0. A user would expect a
filter, and the project's stance elsewhere is to refuse a flag that would do
nothing.

`--dry-run` is worse. `doc/safety.md` says: "Read-only lookups still run for
real". Group lookups, the DHCP file and the Slurm client do, through
`App.ReadRunner`. Commands that read through `App.Runner`, whether directly,
through `App.RunOnRole` or through `App.Executor()`, do not. Under
`--dry-run`, `App.Runner` is the recording transport
(`internal/app/app.go:281`). The read is never made, and the empty recording
is printed as data. Observed with a stub `ssh` that logs its calls:

| Command | Real run | `--dry-run` | What `--dry-run` reports |
| --- | --- | --- | --- |
| `boot status -n exe0001` | ssh called | not called | `bootPath: none`, exit 0 |
| `cinc show -n exe0001` | ssh called | not called | `configured: false`, exit 0 |
| `bmc ping -n exe0001` | ssh called | not called | `no answer`, exit 1 |
| `boot list`, `boot log`, `dhcp log` | ssh called | not called | empty, exit 0 |
| `hca link\|cable\|firmware\|config get`, `node hw` | ssh called | not called | empty fields, exit 0 |
| `slurm node list` | ssh called | ssh called | correct |

A script that passes `--dry-run` through to every call, which is a natural
thing to do in a rehearsal, reads "no boot link" and "not configured" where
the truth is unknown.

### F8. Service shells duplicate login

`boot shell`, `dhcp shell` and `cinc shell` each resolve `services.X.role`
and then do what `login ROLE` does. `pdu shell` is `login` to a derived host.
That makes four leaves for one function, and every new service would add a
fifth.

### F9. Configuration that nothing reads

| Setting | Implies |
| --- | --- |
| `services.mail.*` | Warning users before a reboot, as `cluster-reboot-node` did |
| `services.cinc.archivePath`, `baseCookbook`, `rolesPath` | Building and publishing a configuration archive |
| `services.http.root`, `baseUrl` | The same archive step |
| `services.tftp.logPath` | A GRUB/TFTP log command |
| `services.fabric.guidFormat` | A choice of GUID rule (only `mellanox` exists, and it is hard-coded) |
| `workstation.pager` | Paging long output |

requirements.md explains why R21 is deferred: "a setting that was accepted
and did nothing would let a large fan-out run from the workstation while the
site believed otherwise". The same reasoning applies to each row here.

### F10. GSI conventions in built-in defaults and in code

Defaults (`internal/config/defaults.yaml`): `services.dhcp.interface: ib0`,
`logPath: /var/log/syslog`, `/srv/pxesrv`, `/srv/tftp/grub`,
`/etc/cinc/solo`, `bmc.pdu.nameFormat: "pdu%s-%s"` with user `admin`,
`bmc.redfish.systemPath: /redfish/v1/Systems/1`, and
`fabric.guidFormat: mellanox`. In code: `rowOf` (the row is the first digit
of the rack name), the Mellanox GUID rule, and the `lspci` grep in `node hw`.

A built-in default is a claim about every site. A site that names only the
host role of a service inherits GSI's layout for the rest: `boot list`
searches `/srv/pxesrv/boot` on that host, and `dhcp capture` listens on
`ib0`.

### F11. The agent sees more than the administrator

MCP `describe_nodes` answers "what is wrong with exe0007" in one call:
inventory, the BMC it reaches, Slurm state and reason, and jobs. The CLI's
`node describe` shows the inventory, the names and the groups. `provision status` combines the PXE link, the
power state and ssh, but it sits under `provision`, where nobody looks during
an incident.

### F12. Selection goes through subshells

`-n "$(clusterctl slurm node nodeset drain)"` is the documented way to act
on drained nodes. It costs a second process and a second configuration load,
and it is not available through MCP. A Slurm group source exists only if
each site writes the `exec` source by hand, as `examples/site/cluster.yaml`
does.

### F13. Confirmation is inverted between exec and copy

`exec`, which can run anything as root, does not ask unless `--confirm` is
given. `copy --download`, which reads from the nodes and writes only local
files, always asks. Each choice has a reason (`exec` follows `clush`), but
together they teach the wrong lesson about what is dangerous.

### F14. A flat help page of 22 nouns

`clusterctl --help` lists the nouns alphabetically. `exec`, `login` and
`node`, which get used every day, sit between `dns` and `fabric`, and next to
`cinc` and `pdu`, which are rare. cobra can group the list.

## Proposals

Each proposal names the findings it answers. They are ordered by cost within
each group. Every rename would also need the effects table, the MCP tests,
the manual and `doc/migration.md` changed. The rename itself can keep the old
path as a hidden cobra alias with `Deprecated` set for one minor release,
since `v0.1.0` and `v0.2.0` are published.

### A. Quick wins: no new concepts

**A1. Group the help page (F14).** Use cobra `AddGroup` with the groups
*Everyday*, *Scheduler*, *Hardware*, *Provisioning*, *Access and trust* and
*Tool*. It costs nothing, and it shows the portable core at a glance (F1).

**A2. One meaning per short flag (F6).** Keep `exec -b` (clush). Make
`--bmc` long-only. Drop `-R` from `cinc --run-list`. Make the Slurm owner
filter long-only, as `--user`. Add a test that walks the tree and fails when
one short flag has two long names, so the rule in `root.go` is enforced
rather than stated.

**A3. One effect per command (F5).** Remove the `status` action from `bmc
power`; `bmc status` exists. Split `slurm account shares` into `slurm
account shares [ACCOUNT]` (read) and `slurm account set ACCOUNT
--fairshare N` (change). The `set` verb is also where limits can be set
later. Optionally, make the power actions subcommands
(`bmc power on|off|soft|cycle|reset`). Each would then carry only the flags
that apply to it
(`--batch` and `--stagger` matter for `on` and `cycle`; `--lose-jobs` does
not matter for `on`) and its own risk in the effects table.

**A4. Flags for optional operands, and nothing guessed from shape (F4).**
`boot set [NODESET] --path PATH`; `slurm account add ACCOUNT --org O
--description D`; `slurm user add USER --account A --default-account D`;
`slurm partition list [NAME]`, so `partition` is a group like its siblings.
`pdu shell` takes the rack alone, as `pdu list` does.

**A5. Scope the global flags, and make reads read (F7).** Run every
read-effect command's remote reads through `ReadRunner`, so `--dry-run`
means what `doc/safety.md` says. This is a bug fix and should come first.
Then use the effects table, which already knows which commands read, to
refuse `-y`, `--force` and `--dry-run` on read commands, and to refuse `-n`
on commands that take no node set, or make those commands filter by it
(`pdu list`, `secrets list`, `node attrs`).

**A6. Implement or remove the settings nothing reads (F9).** Removal is
allowed under `v1alpha1`. Keep a setting only if the command that reads it
is planned (`services.mail`, see [C3](#c3-slurm-maintenance-reboot-and-reservations)).

**A7. Defaults that claim nothing (F10).** Service paths, the DHCP capture
interface and the PDU format default to unset, and the command says what to
configure. The GSI values move to `examples/site/`. If
`bmc.redfish.systemPath` is unset, it is discovered from `/redfish/v1/Systems`
(one member) and the result is cached with the pin. `rowOf` becomes a
configured pattern or an inventory field.

**A8. Secrets on their own (secrets gaps).** Move `services.cinc.secrets`
to a top-level `secrets` list. Give each entry an optional `nodes`
expression and allow `{name}` in `source` and `secretRef.key` for secrets
that differ per host.

### B. Restructuring

**B1. Typed login targets instead of four shell commands (F8).**
`login [role/|node/|bmc/|pdu/|service/]NAME [-- CMD]`, where a bare name
resolves as today. `service/dhcp` resolves `services.dhcp.role`, and
`pdu/R02` the PDU of rack R02. `bmc/exe0001` opens an ssh session to the
service processor, which the site already manages host keys for with
`hostkey --bmc`. Completion can offer each prefix. `boot shell`, `dhcp
shell`, `cinc shell` and `pdu shell` become deprecated aliases. Every
service added later gets its shell for free.

**B2. One word per boot mechanism (F3).** The recommended shape:

- `netboot set|unset|status|list|sync|log`: what the boot server offers a
  node. `pxesrv` and `grub` are its two implementations; `grub` gains node
  sets and the `bootPaths` rules.
- `bmc nextboot show|set|unset`: the service processor's one-time boot
  device, with the IPMI fallback (`chassis bootdev`) for hardware without
  Redfish.

The cheaper alternative, if `boot` is to stay: rename only `bmc boot` to `bmc
nextboot`, and fold `boot grub` into `boot set --via grub`.

**B3. One noun for the fabric (F2).** Merge `hca` into `fabric`:
`fabric port state|counters|guid` for the subnet manager's view (the node
may be down), and `fabric adapter link|cable|firmware|config get|set` for
the node's view (the node must be up). The fabric engineer gets one place to
look, and there is room for a second fabric type later.

**B4. A driver seam under the provisioning chain (F1, F2).** Name the
capability in the command (`netboot`, `cm`, `dhcp`). Select the
implementation in the Site document, for example
`services.netboot.driver: pxesrv` or `services.cm.driver: cinc`. Behind it
sits a Go interface whose only implementation is today's code. Build no
second implementation until a site asks for one. The point is that the
command names and the configuration stop promising GSI's toolchain, and a
contribution from another site has somewhere to go. `cinc config|run|show`
becomes `cm config|run|show`.

**B5. Site-defined actions (F1, F2, F9).** An `actions` list in the Cluster
document. Each entry has a name, a target (`nodes` or a role), an argument
vector with `$NODE` substitution (no shell, as in `ExecGroupSource`), an
effect (`read` or `change`), a timeout and a description. Run one with
`clusterctl action run NAME [NODESET]`, and list them with `action list`.

It is the general form of B4 for everything too small to deserve a driver:
`ansible-pull`, `puppet agent -t`, a health check script, `wwctl overlay
build`. It inherits the gate, `--dry-run`, protected hosts, output formats
and exit codes. A `read` action can be offered to the agent through
`read_command`. This is the cheapest way to make the tree useful to a site
whose toolchain differs from GSI's, without GSI maintaining that toolchain.

**B6. Separate push from pull (F13).** `copy` pushes to the nodes and asks.
`collect` pulls into one directory per node and does not ask, unless it would
overwrite local files.

### C. New surface

**C1. `node status NODESET`: triage in one command (F11).** Per node: the
inventory entry, Slurm state, reason and jobs, BMC power and health, and ssh
reachability. With `--check fabric,netboot,hostkey`, also the fabric port,
the boot link and the host key. It returns one table, or one JSON object per
node. `provision status` becomes a preset of it, and the CLI answers the
question MCP `describe_nodes` answers. This is the most useful single
addition for any site.

**C2. Built-in Slurm group sources (F12).** When `slurm.role` is set, offer
`@slurm:PARTITION`, `@slurmstate:drain`, `@slurmfeature:gpu` and
`@slurmresv:NAME` through the existing Slurm client, cached like any group
source. A source the site defines under the same name wins. Then
`-n @slurmstate:drain` replaces the subshell, works through MCP, and `slurm
node nodeset` becomes redundant.

**C3. Slurm maintenance: reboot and reservations.**

- `slurm node reboot NODESET --reason R [--asap] [--next-state resume|down]`
  wraps `scontrol reboot`. It is the generic, scheduler-native way to reboot
  a node set without losing jobs.
- `slurm reservation list|create|delete`, with `--maint` and a node set, for
  maintenance windows.
- Optionally, `--notify` on these, which would give `services.mail` its
  reader.

**C4. Widen bmc to what hardware triage needs.**

- `bmc log [NODESET] [--since]`: the event log through Redfish
  `LogServices`, with `ipmi-sel` as the fallback. A separate
  `bmc log clear` is a change.
- `bmc sensors` and `bmc health`: temperatures, fans, power supplies and the
  health rollup.
- `bmc console NODE`: the serial console, through `ipmiconsole` on the
  `via` host or ssh to the service processor.
- `bmc firmware [NODESET]`: `FirmwareInventory`, read-only, with
  `--dedup`-style collapsing to show the one node that differs.
- `bmc bios get|diff|set`: BIOS attributes. `diff` against a reference node
  or a file shows configuration drift across a set.

**C5. `provision finish`.** Wait for ssh, then `hostkey refresh`, `secrets
push`, the configuration management run, and optionally `slurm node
resume`. Each step can be skipped by flag, and each is idempotent, so the
command can simply be repeated. `provision reinstall --finish` chains the
two halves.

**C6. `node audit`: detect drift.** Compare the inventory with DHCP (MAC and
address), DNS (forward and reverse), Slurm (nodes known to one and not the
other), the host key file (missing entries) and the service processors
(reachable, serial number against `cid`). One finding per line, a non-zero
exit for CI. Every part already exists; only the comparison is new.
Audience: Integrator, Ops.

**C7. Plan and apply for people, and an audit log.** `--plan FILE` on any
change command writes the plan the MCP server already builds: the resolved
node set, the preview and the recorded commands. `clusterctl apply FILE`
checks it again and runs it, as `apply_plan` does. Write every change made
from the CLI to the audit log that MCP already keeps. This serves change
review (four eyes, a change ticket) and answers "who powered off R02" after
an incident.

**C8. One hardware inventory.** `node hw`, `hca firmware` and C4's
`bmc firmware` answer parts of the same question from three places. One
`node inventory [--source os|bmc]` with a common schema (vendor, model,
serial, BIOS, BMC and adapter firmware), with collapsing of identical rows,
makes the fleet's consistency one command.

**C9. PDU around outlets, or not at all.** Put the PDU and outlet of each
node in the inventory, and implement `pdu status` and `pdu outlet
on|off|cycle NODESET` over Redfish `PowerDistribution` or SNMP, through the
gate. If that is not wanted, move `pdu` out of the core and express `pdu
shell` as `login pdu/RACK` (B1).

## What the tree could look like

```
Everyday
  login      [role/|node/|bmc/|pdu/|service/]NAME [-- CMD]
  exec       [NODESET] -- CMD
  copy       SOURCE... DEST                        push, asks
  collect    SOURCE... DIR                         pull, per-node dirs
  node       select | list | describe | status | attrs | groups | rack | fqdn
             inventory | audit
  action     list | run NAME [NODESET]             site-defined

Scheduler
  slurm      node list | nodeset | drain | resume | reboot
             job list | history | summary
             partition list
             reservation list | create | delete
             account list | limits | add | set | coordinator
             user list | add | default

Hardware
  bmc        status | power on|off|soft|cycle|reset | nextboot show|set|unset
             log | sensors | health | console | firmware | bios get|diff|set
             redfish get|post|info | ping | forget | web
  fabric     port state|counters|guid
             adapter link|cable|firmware|config get|set
  pdu        list | status | outlet on|off|cycle   only with outlets in the inventory

Provisioning
  provision  reinstall | finish | status
  netboot    set | unset | status | list | sync | log         driver: pxesrv, grub
  dhcp       hosts | config | log | capture                   driver: isc
  cm         config | run | show                              driver: cinc
  secrets    list | check | push

Access and trust
  hostkey    list | scan | verify | refresh | remove
  tunnel     list | status | start | stop
  dns        lookup | aliases

Tool
  config | doctor | mcp | completion | version
```

## Suggested order

1. **Fix first**: A5 (reads under `--dry-run`). It is a correctness bug in
   a safety feature.
2. **Before other sites are invited**, while breaking changes are cheap:
   A1 to A4, A6 to A8, B1, B2 and B3. These are renames and splits, with no
   new behaviour, done once with deprecated aliases.
3. **Highest value for any site**: C1 (`node status`), C2 (Slurm groups),
   C3 (`scontrol reboot`, reservations), and C4's `log`, `sensors` and
   `console`.
4. **Portability seam**: B5 (actions), then B4 (drivers) as the naming and
   configuration change without a second driver.
5. **When there is demand**: C5, C6, C7, C8, the rest of C4, and C9.
