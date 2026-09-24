<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Adversarial review, September 2026

This review looked for what breaks the promises of
[`doc/safety.md`](../safety.md), [`doc/mcp.md`](../mcp.md), the other design
notes and the manual, and for what an untrusted input can make clusterctl do. It covers commit `ebed487`
("feat(config): write a first configuration with config init"); every path and
line number refers to that commit.

Every issue below was reproduced, or traced through the code, by a verifier who
had been asked to refute it. The high and critical ones were then challenged a
second time by someone else. The last section says how the review was done and
what it could not do.

## Summary

| Severity | Issues | Meaning here |
| --- | --- | --- |
| Critical | 2 | An untrusted input runs code on the workstation or sends the BMC password elsewhere, with no confirmation |
| High | 28 | A safeguard the documentation promises does not hold, or a command acts on hosts nobody named |
| Medium | 79 | A wrong result, exit code or preview with a plausible trigger, or a promise broken with moderate impact |
| Low | 53 | Edge cases, small documentation drift and test gaps |

The 162 issues were merged from 243 confirmed findings. Twelve further findings
were refuted; they are listed at the end so that nobody reports them again
without new evidence.

Where the code was built to resist an attacker, it largely does: the MCP plan
and apply protocol, the Redfish certificate pin, and keeping secrets off command
lines all held up (see [What held up](#what-held-up)). The defects cluster in
six places.

1. **Node names are never checked as host names.** The node set parser accepts
   any character except whitespace, brackets and the operators, and a name the
   inventory does not know is passed on as typed. A name beginning with `-`
   becomes an ssh option (1.1). A `#`, `?`, `/`, `:` or `@` in a name sends a
   Redfish request, with the site's BMC password, to a host and port of the
   caller's choosing (1.2). Both are reachable through the MCP `read_command`
   tool, which asks nobody. One check at selection closes both.

2. **The safety gate is narrower than `doc/safety.md` says.** Protected hosts
   are compared by spelling, so `wlm01.hpc.example.org` or `WLM01` gets through
   (2.1, 2.2). `exec` checks them only with `--confirm`, and skips the gate
   entirely with `--stdin` (2.3, 2.4). `bmc forget` and `boot sync` bypass the
   gate (2.9, 2.16), and `confirmAbove: 0` turns the typed count off (2.11).

3. **The Slurm running-job check is off on every site started from
   `config init`** (3.1). Where it is on, it lets `draining` and `failing` nodes
   through (3.2), fails open (3.5) and is skipped by `provision reinstall`
   (3.3). `scontrol` expands `ALL` behind a preview of one host (3.4).

4. **Several paths select hosts nobody named.**
   - An explicit but empty `-n` falls back to `CLUSTERCTL_NODES` (2.7).
   - `exec` drops a node set written before `--` (2.6), and takes the remote
     command's own options as its flags (2.5).
   - A trailing set operator is dropped (4.1).
   - The group cache is shared across clusters (4.2).
   - The DHCP parser attributes one node's address to another, and reinstall
     then arms that address (5.1 to 5.3).

5. **A configuration layer can remove a protection without a word**: a mapping
   override (9.1), a key in the wrong case (9.2), or a second document of the
   same kind and name (9.3).

6. **The exit code contract does not hold** (section 11).
   - Usage errors exit 1, and a misspelt subcommand exits 0.
   - A failing remote tool exits 3.
   - `exec` never exits 3 or 130, and interrupts are reported as failures.

   A script that branches on these codes, as `doc/safety.md` invites, takes the
   wrong branch.

## Fix first

Ordered by risk and by how much each change closes.

1. **Validate node names where they are selected.** In `App.Select`, check each
   name against the host name alphabet, and put `--` before the ssh
   destination. Build Redfish URLs with `url.URL{Host: ...}`, and refuse a host
   carrying a port, userinfo or URL delimiter. This closes 1.1 and 1.2 and the
   injection path of 1.3. Parsing inventory and DHCP addresses as IP addresses
   when they load closes 1.4.
2. **Compare protected hosts by machine, not by spelling.**
   - Map FQDNs, BMC names, trailing dots and case to the inventory name before
     the gate.
   - Resolve `safety.protectedHosts` against the inventory when the
     configuration loads.
   - Refuse change commands on names the inventory does not know unless
     `--force` is given.

   This closes 2.1, 2.2 and 2.17, and narrows 3.4.
3. **Put every `exec` through the gate.**
   - Run `Gate.Check` on every `exec`, and move the `--stdin` branch behind the
     gate.
   - Stop interspersed flag parsing.
   - Treat the words before `--` as the node set, or reject them.

   This closes 2.3 to 2.6.
4. **Treat an explicit `-n` as final.** Use the flag's `Changed` to tell an
   explicit `-n` from an absent one, and treat an empty one as a usage error.
   This closes 2.7.
5. **Make the Slurm job check hold.**
   - Turn it on whenever `slurm.role` is set.
   - Count `draining`, `failing` and every suffixed busy state as busy, and
     refuse unknown states and names Slurm did not report.
   - Run it in `provision reinstall`.
   - Refuse `ALL` and NodeSet names.

   This closes 3.1 to 3.5.
6. **Reject what the node set parser now drops.** Make a trailing or doubled
   operator a parse error (4.1). Key the group and remote-file caches by site,
   cluster and source (4.2, 5.10).
7. **Replace the line-based DHCP parser.** Use one that tokenises comments,
   braces and statements, and take a boot address only from a declaration named
   exactly after the node. This closes 5.1 to 5.3.
8. **Resolve a reinstall before changing anything.** Resolve every node's
   address, BMC, credential and PXE role before the first change, and make
   `boot unset` and `boot status` see the persistent link. This closes 5.4 to
   5.6.
9. **Refuse configuration that silently removes a protection.** When loading,
   refuse mapping overrides that are not merged key by key, override keys that
   differ from the schema only in case, and a second document of the same kind
   and name. This closes 9.1 to 9.3.
10. **Trust one host key file.** Pin `GlobalKnownHostsFile`,
    `KnownHostsCommand`, `VerifyHostKeyDNS` and `UpdateHostKeys` in the
    generated `ssh_config`. Give each process its own file, and pass it to
    sshuttle. This closes 6.1 to 6.3.
11. **Route `bmc forget` through the gate** and honour `--dry-run` (2.9).
12. **Restore the exit code contract.**
    - Map cobra and pflag errors, and unknown subcommands, to exit 2.
    - Keep exit 3 for failures the transport itself reports.
    - Map cancellation to 130 everywhere.

    This closes most of section 11.

## What held up

- **MCP plan and apply.** A client without elicitation support is refused.
  Every attempt to get a change past the human was refused:
  - the elicitation answer state is random and single-use;
  - a declined, cancelled, unchecked or miscounted answer is refused;
  - concurrent questions invalidate each other;
  - a plan applies once;
  - the pinned flags (`--config`, `--context`, `--set`, `--yes`, `--force`)
    are refused in every position.

  The MCP defects in this report concern what read-only commands can reach, and
  unbounded or unfiltered output, not a way around the confirmation.
- **The Redfish client.** Go's transport does not replay a POST on a reused
  connection, and there is no TLS session resumption. The pin covers the leaf
  certificate on every handshake, and a redirect to another host drops the
  `Authorization` header and fails the pin. 7.10 is the narrower case of a
  redirect on the same host.
- **Secrets off the command line.** The IPMI password never appears on a
  command line: it goes into a configuration file, or through `-f` into a file
  made with `mktemp` under `umask 077`. The dry-run recorder discards standard
  input, so secrets pushed to nodes are neither recorded nor printed. `install`
  replaces a symlinked target rather than following it.
- **The node set engine.** It came through about 17 million fuzz executions,
  from the existing target and a new one, with no failure. A differential test
  against ClusterShell 1.10.1 over 5,000 random expressions diverged only on the
  padding and adjacent-number choices that [`doc/nodeset.md`](../nodeset.md)
  records.
- **The test suite and static analysis.** `go test -race ./...` and
  `go test -count=3 ./...` pass with no race or flake. staticcheck 0.8.1
  reports nothing, and neither does golangci-lint v2 built with Go 1.26 against
  the repository's `.golangci.yml`.

## 1. Untrusted names reach ssh, HTTP and shell scripts

This section covers node names, node addresses and command arguments that reach
an ssh command line, a Redfish URL or a remote shell script without being
checked or quoted. The node set parser accepts any character except whitespace,
brackets and the set operators, and nothing downstream checks a name as a host
name. An MCP agent can therefore reach several of these paths through the
read-only `read_command` tool, with no human confirmation.

### 1.1 Node names beginning with `-` become ssh options

**Critical** · `internal/transport/transport.go:236`, `nodeset/parse.go:202`

`Client.Args` puts the destination before the `--`. When the context sets no
`user:`, the destination is the bare node name, so ssh parses a name beginning
with `-` as an option. `config init` writes exactly that configuration when
`--user` is not given. Nothing rejects such a name: the node set parser accepts
it, and `canonicalize`, `Namer.FQDN` and `App.Node` pass an unknown name
through, with at most a domain appended. `node hw`, `hca link`, `hca cable` and
`hca firmware` are marked read and `-n` is not pinned, so a prompt-injected
agent reaches this through `read_command`. On the command line, `exec` and every
node command take the same path, and a name can also come from a group source or
an inventory file. The outcome depends on the ssh client. A client without the
CVE-2023-51385 host name check (OpenSSH before 9.6, unless the fix was
backported) runs an injected `ProxyCommand` on the administrator's workstation.
OpenSSH 9.6 rejects the displaced remote command as a host name, unless that
command is a single word, as it is with `fanout.commandTimeout` set to `0`.
Whether current EL8 and EL9 packages carry the backport was not checked.

**Shown by:** The context user was removed from a copy of `examples/site` and
the unpatched `OpenSSH_8.9p1 Ubuntu-3` client was set as `ssh.binary`. Both
`node hw -n '-oProxyCommand=touch${IFS}<scratch>/pwned1;#'` and an MCP
`read_command` with the same arguments created the marker file, and the MCP call
returned exit code 1 without asking for confirmation. With OpenSSH 9.6p1 and the
default timeout no file was created, but `--set fanout.commandTimeout=0s exec -y
-n '<same>' -- uptime` created it.

**Fix:** Move the `--` in front of the destination (`ssh -F CFG -T -- DEST
COMMAND`), as `CopyArgs` already does for scp. Reject host and user names that
begin with `-` or fall outside the host name alphabet, in the node set or in
`App.Select` and `Namer`.

### 1.2 Characters in a node name redirect the Redfish request, and the BMC password with it

**Critical** · `internal/redfish/client.go:58`, `internal/naming/naming.go:84`

`Namer.BMC` cuts a node name at its first `.` and substitutes the rest into the
naming rule without checking it. `Client.BaseURL` builds `"https://" + Host`,
and `DoRaw` passes that to `url.JoinPath` and then attaches the site BMC
credential with `SetBasicAuth`. A `#`, `?` or `/` in the name turns the
management domain into a fragment, query or path, a `:` sets the port and an `@`
adds userinfo, so the request goes to a host and port the caller chooses. `bmc
status`, `bmc redfish get` and the other Redfish reads are marked read, so an
agent reaches this through `read_command` with no plan or confirmation. The pin
store trusts the unknown host on first use, and the command reports success. The
target has to be named without a dot. That can be `localhost`, a short name in
the DNS search domain or, where Go uses the libc resolver, a decimal IPv4
literal such as `2130706433`. On a shared workstation, any local user listening
on an unprivileged port can collect the password.

**Shown by:** A scratch MCP test ran a TLS listener on 127.0.0.1 with
`BMC_PASSWORD` set. `read_command ["bmc","status","-n","localhost:PORT#"]`
returned exit code 0 with the BMC `localhost:PORT#.mgmt.example.org` in state
`On`. The listener logged `GET /redfish/v1/Systems/1` with `admin:<password>`,
and the pin file gained a line for that name. The same happened with the `?`,
`/x` and `x@localhost:PORT#` variants, with `bmc redfish get
/redfish/v1/AccountService/Accounts`, and with `2130706433:PORT#` under
`GODEBUG=netdns=cgo`.

**Fix:** Accept as a BMC host only a DNS name or IP literal with no port,
userinfo or URL delimiters. Build the URL with `url.URL{Scheme: "https", Host:
host, Path: path}` and check that the request's host is the expected one. Reject
node names outside the host name alphabet when they are selected.

### 1.3 `fabric state` puts node names unquoted into a root shell script

**Medium** · `internal/cli/fabric.go:142`

`fabric state` writes each node name into the script it runs on the fabric role
with a bare `%s` (`printf '%s %s ' NODE GUID; ...`). The GUID is safe
hexadecimal, but the name is not quoted. The name must be one for which
`nodeGUIDs` finds a MAC: an inventory entry, a DHCP host name or a word in a
`dhcpd.conf` comment. An agent cannot supply such a name itself, so the
precondition is a malicious or mistyped inventory or `dhcpd.conf` entry. Once
such an entry exists, an ordinary group selection such as `-n @gpu` picks it up,
and `fabric state` is marked read, so `read_command` can trigger it. Shell
syntax in the name then runs on the fabric host, which is `root` in the example
site. A stray quote or parenthesis in any name breaks the script for every node
in the set.

**Shown by:** A scratch test added an inventory node `$(touch${IFS}PWNED)` in
class `gpu`. `fabric state -n @gpu` sent a script containing `printf '%s %s '
$(touch${IFS}PWNED) 0x0011220300334477; ...`, and running that script with `sh
-c` created `PWNED`. `node list` also accepted the name
`gpu01;touch${IFS}/tmp/x`.

**Fix:** Quote the name with `shellquote.Quote`, as the boot and provision
scripts do. Validate node names when the inventory loads.

### 1.4 Node addresses are used as file names on the PXE host without validation

**Medium** · `internal/cli/provision.go:598`, `internal/cli/boot.go:44`

`nodeAddress` returns the inventory address as written, or else the
`fixed-address` from `dhcpd.conf`, which ISC dhcpd allows to be a host name.
`provision reinstall`, `boot set` and `boot unset` join that address into a link
path under the PXE root and run `ln -sfn` or `rm -f` as root on the install
host. Only `boot grub` parses the address. A CIDR typo such as `10.0.2.2/24`
stops the script under `set -eu` part way through, leaving some nodes armed and
the rest not. A host name gives a link that the PXE service never looks up,
while the reinstall reports every step ok. Two nodes that share an address make
`boot set` rewrite the other node's link, which arms a node nobody selected. A
`..` in an inventory address places or removes symlinks outside the PXE root,
but only someone who can edit the inventory can do that.

**Shown by:** With exe0002 at `10.0.2.2/24`, `boot set -y -n exe[0001-0003]`
sent `ln -sfn … /srv/pxesrv/10.0.2.2/24`. Run locally, the script failed on that
line after exe0001's link had been made. `fixed-address
exe0005.hpc.example.org;` produced the link
`/srv/pxesrv/exe0005.hpc.example.org`. A node sharing exe0001's address made
`boot set -n exe0006` rewrite `/srv/pxesrv/10.0.2.1`.

**Fix:** In `nodeAddress`, parse each address with `net.ParseIP` before anything
is confirmed or sent, as `addressToHex` already does for GRUB, and reject
duplicate addresses within a set. Validate inventory addresses when the
configuration loads.

### 1.5 `cinc config` writes the URL and run list unquoted into a file sourced as root

**Medium** · `internal/cli/provision.go:307`

`cinc config` writes `CHEF_RECIPE_URL=%s` and `CHEF_RUN_LIST=%s` into
`/etc/cinc/solo` with the raw arguments. `cinc run` then sources that file under
`set -eu` as the remote user, which is root. Ordinary input breaks it. A URL
whose query string contains `&` sends the assignment to the background, and a
run list written with a space runs its second part as a command, so `cinc run`
then fails on every node. A URL containing `$(...)` or backticks runs as root on
every node at the next `cinc run`, and the confirmation shows the string exactly
as typed. The value comes only from the administrator's command line. `cinc
config` is not offered through MCP, where `plan_change` knows only drain and
resume.

**Shown by:** A bash simulation wrote the exact file and ran the exact `cinc
run` script with a stub `cinc-solo`. The URL
`http://installer/cinc/a.tgz?v=1&t=2` gave `CHEF_RECIPE_URL: unbound variable`
(exit 1). The run list `role[base], role[exe]` gave `role[exe]: command not
found` (exit 127). The URL `http://installer/cinc/$(touch PWNED).tgz` created
`PWNED`.

**Fix:** Write each value with `shellquote.Quote` and reject newlines and NUL.
Validate the URL with `net/url` (scheme `http` or `https`, no whitespace).
Better still, have `cinc run` read the file without evaluating it as shell.

### Lower severity

- **1.6 `hca config` splices KEY and VALUE unquoted into a shell script**
  (`internal/cli/fabric.go:382`). The set path builds `mlxconfig ... set
  KEY=VALUE` with `fmt.Sprintf`, so a value such as `1; reboot` runs `reboot` on
  every selected node. The read path does quote the key. The input comes only
  from the operator's own command line and the confirmation shows it as typed.
  Validate KEY and VALUE against a conservative grammar and pass
  `shellQuote(key+"="+value)`.
- **1.7 `dhcp log` splices the log path into `sh -c`; `dhcp capture --seconds 0`
  never ends** (`internal/cli/dhcp.go:162`, `internal/cli/dhcp.go:219`).
  `services.dhcp.logPath` goes unquoted into `grep -a dhcpd %s | tail -n %d`. A
  path with a space greps the wrong files, and one with `;` runs a command on
  the DHCP server, but only someone who can edit the configuration can set it. A
  `--seconds` value of `0` or below drops the `timeout` wrapper, so `tcpdump`
  runs as root until something stops it, which breaks the promise in the help
  text; under `ssh -T` a local Ctrl-C does not reach it. Reject values below 1
  and quote the path.
- **1.8 Read commands let an agent open connections to any host it names**
  (`internal/naming/naming.go:68`). `Namer.FQDN` returns any dotted name
  unchanged. Through `read_command`, an agent can make `hostkey scan` or
  `hostkey verify` connect to port 22 of any host the workstation reaches, with
  any `--timeout`, and make `dns lookup` send data encoded in a name to the site
  resolver. Host key checking stops ssh-based reads before authentication, so no
  credential leaks. Under MCP, limit node names to inventory or group members or
  to the site's domains, and cap `--timeout`.
- **1.9 Naming patterns match unanchored, and an IP address maps to BMC `10`**
  (`internal/naming/naming.go:100`). `MatchString` matches a substring, so
  `gpu[0-9]+` also claims `login-gpu01`, although the schema says the short name
  "must match". `Namer.BMC` cuts at the first `.`, so `bmc status -n
  10.0.2.[1-4]` queries the single host `10.mgmt.example.org`, and the `bmc
  power` confirmation shows only node names. Anchor patterns as `^(?:...)$` or
  document substring matching, and refuse IP literals in `Namer.BMC`.

## 2. The safety gate and the command line

This section covers the safety gate that every change is meant to pass through,
and the command-line parsing that decides which hosts a command reaches. A
defect here sends a command to a host the administrator did not name, or past a
check that the documentation promises.

### 2.1 Protected hosts are compared as typed, so another spelling of the same machine gets through

**High** · `internal/safety/safety.go:81`, `internal/app/app.go:368`,
`internal/mcpserver/read.go:107`

`Gate.Check` intersects the selected set with `safety.protectedHosts` by
spelling. `App.canonicalize` resolves only padding and leaves a name the
inventory does not know exactly as typed. The naming rules then map other
spellings back to the protected machine: `Namer.FQDN` returns any dotted name
unchanged, and `Namer.BMC` drops everything after the first dot. So
`wlm01.hpc.example.org`, `wlm01.example.org`, `wlm01.`, the BMC name
`wlm01.mgmt.hpc.example.org` and a group source that answers with FQDNs all pass
the gate and reach wlm01 or its BMC. The Slurm job check asks `sinfo` about a
name it does not know and lets these through, so only the y/N question remains,
and `-y` answers it. `bmc power`, `slurm node drain` and `exec --confirm` are
all affected. Over MCP, `select_nodes` lists such a name as unknown but not as
protected, and `plan_change` and `apply_plan` accept it. Whether `scontrol` then
acts on the controller depends on Slurm resolving the name.

**Shown by:** `clusterctl --config examples/site bmc power off -n wlm01
--dry-run` is refused with exit 2. With `-n wlm01.hpc.example.org`, `wlm01.`,
`wlm01.example.org`, `wlm01.mgmt.hpc.example.org`, `WLM01` or `10.0.1.1`, the
same command prints `Would power off 1 host` and exits 0. In the test harness,
`bmc power off --ipmi -y -n wlm01.hpc.example.org` sent `ipmipower ...
--hostname wlm01.mgmt.hpc.example.org --off`, and `exec --confirm -y -n
wlm01.hpc.example.org -- systemctl poweroff` ran on wlm01.

**Fix:** Decide protection by machine, not by spelling. Before the gate,
lowercase names, strip a trailing dot and map an FQDN, BMC name or inventory
address to its inventory name, and compare `Namer.FQDNSet` and `Namer.BMCSet` of
both sets. Refuse change commands on names the inventory does not know unless
`--force` is given, and use the same comparison in `select_nodes`.

### 2.2 `protectedHosts` written as an FQDN or in capitals protects nothing

**Medium** · `internal/safety/safety.go:52`

`NewGate` parses each `safety.protectedHosts` entry as a literal node set and
never checks it against the inventory or the naming rules. An entry written as
`wlm01.hpc.example.org` protects only that spelling, and so does `WLM01`. The
FQDN form is the one the `hosts` roles of the same Site document use. Commands
select the inventory name `wlm01`, which does not match, so the host has no
protection at all, and `config validate` reports the configuration valid. The
documented form is the short inventory name, used by both
`examples/site/site.yaml` and the manual, so this needs a misconfiguration.
Nothing reports that misconfiguration.

**Shown by:** In a copy of `examples/site` with the entry `wlm01` changed to
`wlm01.hpc.example.org`, `config validate` printed `7 documents are valid;
context cluster1 resolves`. `bmc power off -n wlm01 --dry-run` printed `Would
power off 1 host: wlm01` and exited 0, and only `-n wlm01.hpc.example.org` was
refused.

**Fix:** When the configuration is loaded, resolve each entry against the
inventory and the naming rules, as in 2.1. Make `config validate` and `app.New`
refuse an entry that names no known node.

### 2.3 `exec --stdin` skips `--confirm`, the protected-host check and the dry-run preview

**High** · `internal/cli/exec.go:88`

The `--stdin` branch reads the payload and returns `runWithPayload(...)` before
the `if confirm || a.DryRun()` block that calls the gate. An explicit
`--confirm` is ignored without notice. There is no question, no protected-host
refusal and no refusal when there is no terminal, although the flag's help
promises to `ask before running, as the destructive commands do`. With
`--dry-run` nothing is sent, because the recorder is the runner, but no preview
is printed either and a protected host is not reported. `--stdin` is how a file
is pushed to every node, so this hits exactly the case where the administrator
asked to be asked.

**Shown by:** In the harness without a terminal, `exec --confirm --stdin -n
exe0001,wlm01 --script 'cat > /etc/motd'` wrote to both hosts, including the
protected wlm01. The same command without `--stdin` was refused with `needs a
confirmation but there is no terminal to ask on`. On a terminal answering `n`,
the run with `--stdin` made 2 calls and printed no question.

**Fix:** Move the `--stdin` branch below the gate block, so that `--confirm` and
`--dry-run` apply to it. Standard input carries the payload, so the answer
cannot be read from it: ask on `/dev/tty`, or require `-y` with `--stdin
--confirm`.

### 2.4 `exec` checks protected hosts only when `--confirm` is given

**High** · `internal/cli/exec.go:99`

`exec` calls `a.Gate.Confirm`, which holds the protected-host check, only when
`--confirm` or `--dry-run` is given. `exec` is classified as a change in
`internal/cli/effects.go`, and `doc/safety.md` says everything that changes
something goes through the gate. Even so, a plain run never looks at
`safety.protectedHosts`. Not asking by default is intended, but skipping the
protected-host list is documented nowhere. The dry run also takes the gate path
and reports a refusal that the real run never makes. `copy`, the matching change
command, always runs the gate.

**Shown by:** In the harness, `exec -n exe0001,wlm01 -- reboot` sent `reboot` to
both hosts and exited 0. `--dry-run exec -n exe0001,wlm01 -- reboot` exited 2
with `run a command on would touch the protected host wlm01; pass --force to do
it anyway`.

**Fix:** Run `a.Gate.Check` on every `exec`, and let only the question depend on
`--confirm`, so that the dry run and the real run make the same decision.

### 2.5 `exec` takes the remote command's options as its own when `--` is omitted

**High** · `internal/cli/exec.go:54`

`exec` accepts any arguments (`cobra.ArbitraryArgs`) and uses pflag's default
interspersed parsing. Without `--`, it takes every positional word as the
command. Any word of the remote command that looks like an `exec` or global flag
is consumed by clusterctl, and the command runs without it. `-r` becomes
`--root`, `-n` replaces the node set, and `-u`, `-y`, `-o`, `-b`, `--force` and
`--dry-run` are consumed the same way. `exec` asks nothing by default, so the
altered command runs at once, and a replaced `-n` can name a protected host,
which `exec` does not check (2.4). `doc/migration.md` maps `cluster-reboot-node`
to `exec -- shutdown -r`, and the `clush` habit of writing the command straight
after the node set makes it easy to leave out the `--`.

**Shown by:** In the harness, `exec -n exe0001 shutdown -r now` ran `shutdown
now` as root on exe0001, which powers it off instead of rebooting it. `exec -n
exe0001 grep -n wlm01 /etc/hosts` ran `grep /etc/hosts` on the protected wlm01,
and `exec -n exe[1-4] tail -n 100 /var/log/messages` targeted a host called
`100`.

**Fix:** Call `cmd.Flags().SetInterspersed(false)` so that the first positional
word ends option parsing, or require `--` whenever a command is given.

### 2.6 `exec` drops a node set given before `--` and runs on `CLUSTERCTL_NODES` instead

**High** · `internal/cli/exec.go:56`, `internal/cli/exec.go:66`

When `--` is present, `exec` keeps only `args[ArgsLenAtDash():]` and selects
with `a.Select("")`. Words before `--` are dropped without a message, and the
node set falls back to `-n` or `CLUSTERCTL_NODES`. Every other node command
takes a positional `NODESET`, and `clush` users write the nodes first, so `exec
NODES -- CMD` is a natural thing to type. The node-sets guide recommends
exporting `CLUSTERCTL_NODES` and then running `exec -- uptime`. With the
variable set, `clusterctl exec exe0002 -- reboot` reboots the whole session set
with no question and exits 0. Words of the command itself are lost the same way:
`exec -n exe0001 sudo -- reboot` runs `reboot` without `sudo`, and `exec -n
exe0001 reboot -- now` runs `now`. With neither `-n` nor the variable set, the
command stops with exit 2.

**Shown by:** `CLUSTERCTL_NODES='exe[0001-0010]' clusterctl --config
examples/site exec exe0002 --dry-run -- reboot` printed `Would run a command on
10 hosts: exe[0001-0010]` and exited 0. The same run in the harness without
`--dry-run` made 10 `reboot` calls and printed no question.

**Fix:** When `ArgsLenAtDash()` is greater than 0, treat `args[:at]` as the node
set, as `selection()` does elsewhere, or reject those words with exit 2. Refuse
a positional node set given together with `-n`.

### 2.7 An explicit but empty `-n` falls back to `CLUSTERCTL_NODES`

**High** · `internal/cli/root.go:86`

`nodesFromFlagOrEnv` tests `r.nodes != ""`, so it cannot tell `-n ""` from no
`-n` and falls back to `CLUSTERCTL_NODES`. This contradicts three documents.
`doc/configuration.md` says the variable applies when `-n` is not given. The
node-sets guide says `-n` always wins over it. Step 1 of `doc/safety.md` says an
empty selection is a usage error. The help of `slurm node nodeset` and the Slurm
guide show `-n "$(clusterctl slurm node nodeset drain)"`, which yields exactly
`-n ""` when no node is in that state. With the variable exported, a command
meant for no host acts on the whole session set, and with `-y` it runs. `-n " "`
is correctly refused.

**Shown by:** With `CLUSTERCTL_NODES='@rack:R02'`, `clusterctl --config
examples/site exec --dry-run -n "" -- uptime` printed `Would run a command on 10
hosts: exe[0001-0010]` and exited 0, and `bmc power off --dry-run -n ""` did the
same. `-n " "` gave `no nodes were selected; pass -n or set CLUSTERCTL_NODES`
with exit 2.

**Fix:** Use the persistent flag's `Changed` to tell an explicit `-n` from an
absent one. Treat an explicit empty `-n` as a usage error without reading the
environment.

### 2.8 Positional node arguments replace `-n`, and a repeated `-n` keeps only the last

**Medium** · `internal/cli/helpers.go:63`, `internal/cli/root.go:137`

`selection()` joins the positional arguments and passes them to `a.Select`,
which then ignores `-n` without a warning. `-n` is a plain string flag, so `-n A
-n B` keeps only `B`, whereas `clush -w` accumulates repeated options. In both
cases some of the named hosts are silently left out. No unnamed host is ever
added, and with `-y` there is no preview in which to notice the loss. This
affects every command that takes `[NODESET]`, among them `bmc power`, `provision
reinstall`, `secrets push` and `hostkey`.

**Shown by:** `bmc power off -n exe0001 exe0002 --dry-run` and `bmc power off -n
exe0001 -n exe0002 --dry-run` both printed `Would power off 1 host: exe0002` and
exited 0.

**Fix:** Reject a positional set combined with an explicit `-n` with exit 2, or
take the union of both and show it. Make `-n` a string array whose values are
unioned, or reject a repeated `-n`.

### 2.9 `bmc forget` ignores `--dry-run` and the gate and removes certificate pins

**High** · `internal/cli/bmc.go:808`, `internal/cli/bmc.go:804`

`bmc forget` is classified as a change in `internal/cli/effects.go`, but it
calls `PinStore.Remove` directly: it never checks `a.DryRun()` and never calls
`a.Gate.Confirm`. `--dry-run` therefore removes the pin. There is no preview, no
protected-host check and no refusal without a terminal. `hostkey remove`, the
equivalent command for ssh host keys, does go through the gate. The pin is the
only trust anchor for a BMC's certificate. After it is removed, the next Redfish
connection records whatever certificate it is shown and sends the BMC credential
to that host, so an attacker on the management network at that moment gets the
credential. Separately, `forget` passes its arguments to `Namer.BMC` without
selecting them. `bmc forget exe1` therefore misses the pin stored for `exe0001`.
The BMC host that the pin-mismatch error tells the administrator to pass gets
`naming.bmcPrefix` added a second time. In both cases nothing is removed, yet
the command prints `forgot the certificate of ...` and exits 0.

**Shown by:** The pin file held entries for `exe0007.mgmt.hpc.example.org` and
`exe0008.mgmt.hpc.example.org`. `clusterctl --config examples/site --set
bmc.redfish.pinStore=<pins> --dry-run bmc forget exe0007` printed `forgot the
certificate of exe0007.mgmt.hpc.example.org` and exited 0, and the file
afterwards held only the exe0008 line. `bmc forget exe1` printed `forgot the
certificate of exe1.mgmt.hpc.example.org` and left the exe0001 pin in place.

**Fix:** Select the arguments with `a.Select` and map them with `Namer.BMC`, as
the other BMC commands do, and also accept a BMC host that is recorded in the
pin file. Pass the removal through `a.Gate.Confirm` with the node set and stop
on a dry run. Print the fingerprint being dropped, and say when nothing matched.

### 2.10 `--force` lifts host protection as well as the Slurm check

**Medium** · `internal/cli/bmc.go:407`, `internal/safety/safety.go:78`

`bmc power` runs the Slurm job check before `Gate.Confirm`. Its refusal names
only the busy nodes and says `pass --force to lose the jobs`. `--force` is also
the flag that makes `Gate.Check` skip the protected-host check, so an
administrator who follows that advice also powers off any protected host in the
set. The protected-host refusal, which `doc/safety.md` says names the host, is
never shown. After `--force` the preview gives no sign that a protected host is
included, and with `-y` nothing mentions it at all. `doc/safety.md` documents
`--force` as the override for both checks, so the defect is the misleading
refusal and the unnamed host, not the override itself.

**Shown by:** In the harness, `sinfo` answered `exe0001 allocated` and `wlm01
idle`. `bmc power off -n exe0001,wlm01 -y` exited 2 with `exe0001 is running
Slurm jobs; drain them first, or pass --force to lose the jobs`, which does not
name wlm01. With `--force` on a terminal, the prompt read `About to power off 2
hosts: exe0001,wlm01` with no protection warning.

**Fix:** Give the Slurm check its own override, for example `--lose-jobs`.
Otherwise run the protected-host check first and make each refusal name every
check that `--force` would lift. When `--force` lifts the protection, name the
protected hosts in the preview.

### 2.11 `confirmAbove: 0` turns off the typed count

**Medium** · `internal/safety/safety.go:158`,
`internal/apis/v1alpha1/types.go:290`

The gate requires the typed count only when `g.ConfirmAbove > 0 && count >
g.ConfirmAbove`, so `safety.confirmAbove: 0`, or any negative value, turns the
typed count off. The JSON Schema description (`Ask for the count to be typed
above this many hosts`), step 4 of `doc/safety.md` and the manual all read as if
`0` meant that the count is always typed. Only a Go comment says otherwise. A
cautious site that sets `0` gets the reverse: a plain `y` confirms an action on
any number of hosts. The schema sets no minimum, so a negative value validates,
and `safety.powerOnBatch: 0` turns batching off in the same way.

**Shown by:** A Workstation override of `safety.confirmAbove: 0` was layered
over `examples/site`. `slurm node drain reason -n exe[0001-0100]` on a terminal
then accepted `y` and drained 100 hosts. With the default of 8, the same `y` was
refused and nothing was sent. `config validate` accepted `confirmAbove: -5`.

**Fix:** Make `0` mean that the count is always typed, or say in the schema
description and the manual that `0` turns it off. Add a minimum to
`confirmAbove` and `powerOnBatch` in the schema.

### 2.12 Arguments and output expressions are checked only after the gate or after the change

**Medium** · `internal/cli/bmc.go:118`, `internal/cli/bmc.go:224`,
`internal/output/output.go:67`

`bmc power` reads its action without validating it and checks it only when it
sends. The dry run prints `Would power offf 1 host` and exits 0. The real run
first asks Slurm and, over IPMI, resolves the BMC credential. The IPMI backend's
rejection is then wrapped as `exitcode.Transport`, so a typo exits 3, the code
for a host that could not be reached. `reboot` is accepted over Redfish, is not
in `ipmi.Actions()` or the help, and exits 3 over IPMI. `boot unset` resolves
addresses only after the confirmation, and `secrets push` decrypts only after
it. Their dry runs therefore approve runs that then stop with exit 2, although
they stop before any change. `-o jq=...` and `-o jsonpath=...` expressions are
compiled only when the result is printed, and destructive commands print after
they have acted. A typo in the expression turns a successful change into exit 1,
the per-host results are lost, and a wrapper that retries on exit 1 repeats the
change. A jsonpath such as `{[']}` panics with exit 2.

**Shown by:** `bmc power offf --ipmi -y -n exe0001` sent `sinfo` and then exited
3 with `unknown power action "offf"; expected one of status, on, off, cycle,
reset, soft`. With a logging fake `ssh`, `clusterctl --config examples/site exec
-n exe0001 -o 'jq=.[' -- touch /tmp/flag` sent the command and then exited 1
with `invalid jq expression`. With `-o "jsonpath={[']}"` it sent the command and
then panicked in `output.parseBracket` with exit 2.

**Fix:** Before `Gate.Confirm` and before any Slurm or credential lookup,
validate the action against `ipmi.Actions()` and `resetTypeFor`, resolve the
node addresses and check that the secrets can be decrypted. Classify backend
argument errors as `exitcode.Usage`. Compile jq and jsonpath expressions in
`output.ParseFormat`, and recover panics in the query code.

### 2.13 Dry runs that fail for runs that would work

**Medium** · `internal/cli/slurm.go:623`, `internal/app/remote.go:40`,
`internal/app/app.go:208`

Under `--dry-run`, `app.New` replaces `a.Runner` with an empty recorder, so
every read-only lookup that goes through `a.Runner` gets an empty answer. `slurm
user add` asks `getent` through it before the gate and reads the empty reply as
"no such user". It then stops with `the cluster does not know a user account`
and exit 2, for every user. `App.RemoteFile` reads `dhcpd.conf` the same way.
`provision reinstall`, `boot set` and `boot grub set` therefore fail with `no
address is known for exe0007` for a node whose address comes from DHCP, unless a
real command cached the file in the last two minutes. Group lookups deliberately
keep the real transport, but these reads do not. Both failures are fail-closed,
but they break the rehearsal that `doc/migration.md` recommends.

**Shown by:** With a fake `ssh` that answered `getent` for alice, `slurm user
add alice proj --dry-run` exited 2 with that message, while `slurm user add
alice proj -y` printed `user alice associated`. With a recorder answering `cat
/etc/dhcp/dhcpd.conf` with a host block for exe0007, `provision reinstall -n
exe0007 --dry-run` failed with `no address is known for exe0007; set it in the
inventory or in DHCP`, while `boot set -y -n exe0007` succeeded.

**Fix:** Give `App` a read runner that stays real during a dry run, as the group
resolver's does, and use it for `RemoteFile` and `HasPosixUser`. Alternatively,
skip these lookups in a dry run and say so in the preview.

### Lower severity

- **2.14 Dry runs that differ from the real run** (`internal/cli/bmc.go:377`,
  `internal/app/app.go:218`, `internal/cli/doctor.go:199`,
  `internal/cli/slurm.go:144`). Under `--dry-run` the Slurm job check and
  `doctor --remote` read the recorder's empty reply as success, so `bmc power
  off -n exe0007 --dry-run` prints `Would power off` for a busy node and `doctor
  --remote --dry-run` reports every role `ok` without contacting any. Group
  sources such as `@slurm:main` are resolved over real `ssh`, although
  `doc/safety.md` and the manual say a dry run sends nothing, and `slurm node
  drain` checks its reason only after the confirmation, so a blank reason
  previews and exits 0 under `--dry-run` and exits 1 (a target failed) instead
  of 2 in a real run. Choose one rule for read-only lookups in a dry run and
  document it: run them for real, or skip them and name the skipped checks in
  the preview; validate the drain reason before the gate with `exitcode.Usage`.
- **2.15 A forgotten or unquoted drain reason turns node names into the reason**
  (`internal/cli/slurm.go:145`). `slurm node drain` takes the first argument as
  the reason and the rest as a node set that wins over `-n`, and an empty set
  falls back to `CLUSTERCTL_NODES`. With `CLUSTERCTL_NODES='@rack:R02'`, `slurm
  node drain exe0007 -y` drained `exe[0001-0010]` with the reason `exe0007`, and
  `slurm node drain ticket 4711 DIMM -n exe0007` previewed a drain of
  `4711,DIMM` and ignored `-n`. Refuse a reason that parses as a known node or
  group, and refuse positional nodes together with `-n`.
- **2.16 `boot sync` changes the PXE host without the gate**
  (`internal/cli/boot.go:344`). `boot sync` is classified as a change, but it
  runs `git -C <repoPath> pull --ff-only` on the PXE host with no preview, no
  question and no refusal without a terminal; it honours only `--dry-run`. Ask
  before the pull, and require `-y` when there is no terminal.
- **2.17 One machine named twice is reset twice in parallel**
  (`internal/cli/bmc.go:315`). `forEachBMC` builds one Redfish client and one
  request per spelling. `-n exe0001,exe0001.hpc.example.org,exe0001.` therefore
  sends three concurrent resets to `exe0001.mgmt.hpc.example.org`, `exec` runs
  twice on the host, and the preview counts 16 hosts for 8 machines, while the
  IPMI path folds the names and sends once. Resolve targets to machine identity
  before the preview, and deduplicate or refuse such a set.
- **2.18 The manual's `protectedHosts` example with a group makes every command
  fail** (`internal/safety/safety.go:52`). The access guide lists
  `"@inventory:infra"` under `safety.protectedHosts`, but `NewGate` parses the
  entries without the group resolver. Every command, `config validate` included,
  then exits 2 with `group @inventory:infra cannot be resolved: no group source
  is configured`. Pass the resolver, which is built just before the gate, to
  `NewGate` and fail closed when resolution fails, or remove the example.

## 3. The Slurm running-job check

This section covers the check that asks Slurm, before a power action, whether a
node is running a job. It is the only safeguard between a routine power action
and the loss of other users' jobs, and several paths leave it off or let busy
nodes through.

### 3.1 The check is off unless `safety.slurmAware` is set, and neither the defaults nor `config init` set it

**High** · `internal/cli/bmc.go:364`, `internal/config/defaults.yaml:51`,
`internal/config/scaffold/site.yaml.tmpl:43`

`checkSlurmIdle` returns without a word when `safety.slurmAware` is unset or
false, and also when `slurm.role` is empty. `internal/config/defaults.yaml` does
not set `slurmAware`. Neither does the Site document that `config init` writes,
although the cluster document it writes sets `slurm.role: login`. Only
`examples/site/site.yaml` and the access guide set it. `doc/safety.md`, the
power and Slurm guides and the `bmc power` help all describe the refusal as on
by default; the help says a busy node is refused `unless the workload manager
check is turned off`. On every site started from `config init`, `bmc power off`,
`cycle`, `reset` and `soft` therefore go straight to the question for nodes that
are running jobs, and with `-y` the jobs are lost.

**Shown by:** `config init` followed by `config explain safety.slurmAware`
printed `nothing is set at "safety.slurmAware"`. In the harness, with `sinfo`
answering `exe0007 allocated`, `bmc power off -n exe0007` on that configuration
went straight to `About to power off 1 host` without sending `sinfo`. On
`examples/site` the same command was refused.

**Fix:** Treat an unset `slurmAware` as true whenever `slurm.role` is set, or
set `slurmAware: true` in `defaults.yaml` and in the scaffold. When the check is
skipped for any reason, say so before the question.

### 3.2 `draining`, `failing` and flagged states are let through although they run jobs

**High** · `internal/cli/bmc.go:393`

`checkSlurmIdle` strips only `*~#$@+` from the `%T` state. It counts a node as
busy only for `allocated`, `alloc`, `mixed`, `mix`, `completing` and `comp`.
Slurm reports a node that has the drain flag and still runs jobs as `draining`,
and a failed node that still has a job as `failing`; `internal/slurm/slurm.go`
lists both states itself. The suffixes `-`, `!`, `%` and `^` are not stripped,
so `mixed-`, `allocated^`, `allocated%` and `mixed!` also pass. The refusal
message tells the administrator to `drain them first`. Right after the drain the
node reports `draining`, so the documented next step switches the check off, and
the job is lost after an ordinary y/N.

**Shown by:** In the harness, with `sinfo` answering `exe0007 <state>`, `bmc
power off -n exe0007` refused `allocated`, `mixed` and `allocated*`. It reached
the question for `draining`, `draining*`, `draining@`, `failing`, `mixed-`,
`allocated^`, `allocated%`, `mixed!` and `completing%`. With `--ipmi -y`, the
power-off script was sent for each of these states.

**Fix:** Strip every Slurm suffix character (`*~#!%$@^-+`), count `draining` and
`failing` as busy, and refuse any state the check does not recognise. Better
still, ask `squeue` for the running jobs on the nodes instead of inferring them
from the state.

### 3.3 `provision reinstall` force-restarts nodes without the check

**High** · `internal/cli/provision.go:561`

`provision reinstall` sets a PXE boot override and sends a Redfish
`ForceRestart` to every node. It never calls `checkSlurmIdle`, whose only caller
is `bmc power`, and it does not drain the nodes either. The group help says each
step is the command of the same name. The manual's step-by-step version uses
`bmc power reset`, which refuses a node running a job. The confirmation line
`everything on these machines is lost` refers to the disks, not to other users'
jobs on those nodes. With `safety.slurmAware: true`, as in the example site, a
reinstall of `@rack:R02` force-restarts nodes that are running jobs.

**Shown by:** In the harness, with `sinfo` answering `exe0001 allocated`,
`provision reinstall -n exe0001 -y` sent no `sinfo`, wrote the PXE link and went
on to the BMC step. With the same replies, `bmc power reset -n exe0001 -y` was
refused with `exe0001 is running Slurm jobs; drain them first, or pass --force
to lose the jobs`.

**Fix:** Call `checkSlurmIdle(a, ns, "reset")` before the gate unless
`--no-reset` is given, with `--force` as the override.

### 3.4 `scontrol` expands `ALL` and NodeSet names, while the preview shows one unknown host

**High** · `internal/slurm/slurm.go:175`, `internal/slurm/slurm.go:182`,
`internal/mcpserver/plan.go:268`

`Client.Drain` and `Client.Resume` pass `ns.Hostlist()` to `scontrol update
nodename=...`. slurmctld does not read that value as a plain host list.
`update_node()` calls `nodespec_to_hostlist()`, which expands `ALL`, in any
case, to every node and replaces a `NodeSet` name from `slurm.conf` with its
members. clusterctl accepts such a word as one host the inventory does not know,
so the gate previews, counts and checks a single host. The typed count above
`safety.confirmAbove` never applies, and protected hosts that are Slurm nodes
are not caught. `slurm node resume -n ALL` therefore resumes every drained or
down node in the cluster after one `y`, and `-n gpunodes`, with the `@`
forgotten, drains every member of that NodeSet. MCP `plan_change` and
`apply_plan` accept the same names.

**Shown by:** In the harness, `slurm node resume -n ALL`, answered with `y`,
printed `About to resume 1 host: ALL` and sent `scontrol update nodename=ALL
state=resume`. `slurm node drain maint -n gpunodes` sent `nodename=gpunodes`.
The Slurm behaviour was confirmed in the 23.11.4 source (`nodespec_to_hostlist`
in `read_config.c`, `update_node` in `node_mgr.c`).

**Fix:** Before any `scontrol update`, refuse names the inventory does not know,
or confirm the set against Slurm: `sinfo -h -N -o %N -n LIST` must return
exactly the set. At the least, reject `all` in any case and names equal to a
Slurm NodeSet, both in the CLI and in `plan_change`.

### 3.5 The check fails open

**Medium** · `internal/cli/bmc.go:387`, `internal/cli/bmc.go:584`

When `sinfo` fails, the check is skipped with a note, which is documented. Apart
from that case, the check never compares the names `sinfo` returns with the set
it asked about. A requested node that `sinfo` does not list counts as idle, and
so does a line with fewer than two fields, and no message is printed. `sinfo -n`
silently drops names it does not match. This happens when the Slurm node names
differ from the inventory names in padding, domain or case, and with the aliases
of 2.1. `bmc redfish post` can send a `ComputerSystem.Reset` action and never
asks Slurm at all.

**Shown by:** With `sinfo` answering nothing and exit 0, `bmc power off --ipmi
-n exe[1-2] -y` sent `sinfo -h -N -o '%N %T' -n exe[0001-0002]` and then the
IPMI script. It reported both nodes `ok` and printed no note.

**Fix:** Compare the returned names with the requested set. Treat a requested
node that Slurm did not report as unknown and name it before the question,
instead of counting it as idle. Run the same check in `bmc redfish post` when
the path is a reset action.

### Lower severity

- **3.6 Long host lists exceed the argument limit and skip the check**
  (`nodeset/nodeset.go:292`, `internal/cli/bmc.go:382`). `NodeSet.Hostlist()`
  expands the whole set as soon as one name has two numeric parts, such as
  `r01n001` or a BMC FQDN in `mgmt.dc2.example.org`, and the transport refuses
  any argument over 131072 bytes. At about 4,600 such BMCs `bmc power --ipmi`
  exits 3, and above about 16,400 two-dimensional node names the `sinfo` call is
  refused, so the job check prints `could not ask Slurm about these nodes;
  continuing without the job check` and the power action goes ahead. Fold per
  pattern, folding at least the last dimension, send long lists over standard
  input, do not let an oversized `sinfo` call disable the check, and correct
  `doc/nodeset.md`, because `scontrol` 23.11 and `ipmipower` 1.6.13 both parse
  several dimensions.
- **3.7 `Hostlist` emits step syntax that Slurm and FreeIPMI do not parse**
  (`nodeset/nodeset.go:296`). For a one-dimensional set, `Hostlist()` returns
  `ns.fold()`, which honours `WithAutostep`, so such a set renders as
  `exe[1-7/2]`, which neither parser accepts. clusterctl never sets autostep, so
  only library users are affected. Fold with autostep disabled in `Hostlist()`.

## 4. Node sets, groups, naming and inventory

This section covers how clusterctl turns an expression, a group or a typed name
into hosts, and how the inventory and the naming rules turn those hosts into
addresses. A defect here sends a correct command to the wrong machines, often
behind a preview that looks right.

### 4.1 A trailing or doubled set operator is silently dropped

**High** · `nodeset/parse.go:80`

`flush()` returns early when the pending term is empty, so the pending operator
is never applied. The next operator then overwrites it at
`nodeset/parse.go:118`. As a result a trailing `&`, `!` or `^` is ignored, a
doubled operator such as `&&` is accepted, and `A!,B` turns into the union
`A,B`. ClusterShell, which `doc/nodeset.md` names as the reference, rejects all
of these with "missing nodeset operand". The usual trigger is command
substitution. When no node is in the state, `slurm node nodeset STATE` prints an
empty line and exits 0, so `-n "@rack:R02&$(clusterctl slurm node nodeset
idle)"` becomes `@rack:R02&`. That selects the whole rack, and with `-y` the
action runs on every host in it, which breaks `doc/safety.md` item 1 ("an empty
selection is a usage error"). An intersection with a group reference such as
`@rack:R02&@slurm:idle` is not affected, because an empty group is an error.

**Shown by:** Against `examples/site`, `node select '@rack:R02&' --expand`
printed all ten hosts of R02 and exited 0, and `'exe[1-10]!,exe5'` kept
`exe0005`. With `IDLE=""`, `bmc power off --dry-run -n "@rack:R02&$IDLE"`
printed `Would power off 10 hosts: exe[0001-0010] through Redfish`.

**Fix:** Record whether an operand followed each operator. Return an error when
`&`, `!` or `^` is followed by another operator, by `,` or by the end of the
expression. Add table tests for `A&`, `A!`, `A!,B` and `A&&B`.

### 4.2 The group cache is shared across contexts, clusters and sites

**High** · `internal/groups/groups.go:404`, `internal/app/remote.go:65`

The on-disk cache of an exec group source is
`<CacheDir>/groups/<source>-<kind>-<group>.json`. `CacheDir` is the global
`$XDG_CACHE_HOME/clusterctl`, which every context, cluster and site shares, and
so does `clusterctl mcp`. The key contains neither the cluster, the target host
nor the command. Suppose two clusters define a cached exec source of the same
name, such as the manual's own `slurm` with `cacheTtl: 60s`, and have partitions
of the same name. A lookup in one context is then answered from the other
context's cache for the whole TTL, with no remote call. The naming rules resolve
those names to real hosts, so the next command acts on the other cluster's
nodes, and the preview gives no sign that they are foreign. The file name also
turns `/` and space into `_`, so `gpu/a100` and `gpu_a100` share one entry.

**Shown by:** Two command trees shared one cache directory. `--context cluster1
node select @slurm:main` asked `login.hpc.example.org` and printed
`exe[0001-0010]`. `--context cluster2 node select @slurm:main` then made no
remote call and printed `exe[0001-0010]`, where a fresh cache gave
`sub[0001-0002]`. `bmc power off --dry-run -n @slurm:main` in cluster2 printed
`Would power off 10 hosts: exe[0001-0010] through Redfish`.

**Fix:** Put the cluster and a hash of the resolved target host and argv into
the cache path, and hash the group name instead of replacing characters in it.
Scope `remoteCachePath` the same way. It is keyed only by role and path.

### 4.3 Inventory entries with different padding create phantom duplicate nodes

**High** · `internal/inventory/inventory.go:95`

`apply` keys records by the literal expanded name. The docs say that `exe1` and
`exe0001` are one host and that a later entry refines an earlier one. In fact a
refinement written as `exe1` after `exe[0001-0010]` creates a second record.
Every selection goes through `Inventory.Resolve` and becomes `exe0001`, so the
refined address, MACs, `bootPath` and attributes are silently ignored. The
attribute groups also disagree: a node reclassified as `spare` stays in `@exe`
and also appears in `@spare`. The reverse spelling does more damage. With
`exe[1-10]` refined by `exe0001`, the rebuilt set takes width 4, every selection
is renamed to a padded name the inventory does not hold, and `node list exe5`
panics (see 4.6). `config validate` accepts both inventories.

**Shown by:** With entries `exe[0001-0010] {class: exe}`, `exe1 {class: spare}`
and `exe2 {address: 10.0.2.2}`, `node list` reported `13 nodes`. `node select
@spare` gave `exe0001` while `@exe` still gave `exe[0001-0010]`, and `boot set
-y -n exe2` failed with `no address is known for exe0002`.

**Fix:** Before applying an entry, canonicalise each expanded name against the
nodes already known. Alternatively, reject two entries whose names differ only
in padding.

### 4.4 Inventory `bmcAddress` is never used, and reinstall sends BMC credentials to the node's own name

**High** · `internal/app/bmc.go:84`, `internal/cli/bmc.go:56`

`bmcAddress` is documented as "the address of its service processor, when it
cannot be resolved from the name", but the code only stores it and shows it in
`node describe`. `RedfishClient`, `bmcSet`, `node fqdn --bmc` and the IPMI paths
all use `Namer.BMC` instead. When the matching naming rule has no `bmc`
template, `Namer.BMC` returns the node's short name. The scaffold written by
`config init` has exactly such a rule. In that case `provision reinstall` and
`provision status` send the site BMC account as HTTP Basic auth to
`https://<node>` and pin whatever certificate answers there. The reset of
`provision reinstall` never reaches the recorded service processor. `provision
status` is a read command, so an MCP agent can trigger it through `read_command`
without confirmation. With a `bmc` template, the derived name still takes
precedence over `bmcAddress`, so a stale DNS record sends power actions to
another device. For Redfish an existing pin catches that case, but IPMI has no
pin.

**Shown by:** The test config had a rule `{match: {prefixes: [gpu, localhost]},
fqdn: "{name}.{domains.hpc}"}` and `bmcAddress: 127.0.0.2` for `localhost`.
`provision status -n localhost` sent `GET /redfish/v1/Systems/1` with
`admin:s3cret` to the server standing in for the node on `127.0.0.1:443`,
recorded a pin for `localhost`, and sent nothing to the `bmcAddress`. With a
`bmc` template and `bmcAddress: 10.9.0.77`, `bmc power cycle --ipmi -y -n
exe0003` ran `ipmipower --hostname exe0003.mgmt.hpc.example.org --cycle`.

**Fix:** Prefer the inventory's `bmcAddress` in `RedfishClient` and `bmcSet`,
and pin its certificate under that address. When no BMC name can be derived,
refuse rather than fall back to the node name (see 4.12).

### 4.5 One display width per pattern renames hosts

**Medium** · `nodeset/nodeset.go:129`, `internal/inventory/inventory.go:50`

A `NodeSet` keeps one display width per pattern dimension. `Canonical` renders
the member at that width instead of returning a name the set was given. It is
documented design that `exe1` and `exe01` name one host, but two consequences
are not. First, mixed widths in the inventory, such as `exe[0001-0010]` plus a
stray `exe11`, make `Canonical("exe11")` return `exe0011`, a name the inventory
does not hold. `exec` and `node fqdn` then target `exe0011.hpc.example.org`,
while `node describe exe11` reports `exe11.hpc.example.org`. Second,
`Inventory.New` accepts two machines whose names differ only in padding, such as
`lab1` and `lab01`. `node describe lab1` shows the `lab1` record, but `exec -n
lab1` and `bmc power off -n @lab` act on `lab01`.

**Shown by:** On a copy of `examples/site` with `exe11`, `lab1 {class: lab}` and
`lab01 {class: labprod}` added, `bmc power off --dry-run -n @lab` printed `Would
power off 1 host: lab01`. `exec --dry-run -n exe11 -- uptime` printed `Would run
a command on 1 host: exe0011`.

**Fix:** Make `Canonical` return a name the set actually holds. Make
`Inventory.New` reject names that are equal once padding is ignored, and warn
about mixed widths within one pattern.

### 4.6 `node list` crashes on mixed padding widths

**Medium** · `internal/inventory/inventory.go:192`

`Inventory.Select` appends `inv.nodes[canonical]` without checking that the key
exists. With mixed widths in one pattern, `Canonical` returns a re-padded name
that is not a key (see 4.5), so `Select` appends a nil `*Node`. `node list` then
dereferences it at `internal/cli/node.go:68` and panics. One unpadded entry in
an otherwise padded inventory is enough to trigger this. `node list` can also be
reached from MCP through `read_command`.

**Shown by:** With `exe11` added to `exe[0001-0010]`, `node list exe11` and
`node list @exe` both panicked with `invalid memory address or nil pointer
dereference` in `newNodeListCommand` and exited 2.

**Fix:** In `Select`, check `n, ok := inv.nodes[canonical]` and treat a miss as
unknown. Fixing 4.5 removes the cause.

### 4.7 Expansion limits are checked too late

**Medium** · `nodeset/parse.go:211`, `nodeset/parse.go:89`

`parsePattern` builds every bracket of a term in full, each up to 2²⁰ values,
and checks the cross product only after the loop. A term with many brackets
therefore allocates about 13 MB per bracket before it is refused. The expression
cap limits only the running result, not the total work, so
`a[0-1048575]!a[0-1048575],` repeated any number of times is accepted, and each
repetition costs seconds of CPU and hundreds of megabytes. `doc/nodeset.md`
promises that a typo is reported "rather than exhausting memory". Expressions
come from the command line, from remote Slurm output, and from MCP clients
(`select_nodes`, `describe_nodes`, `plan_change`, and `-n` in `read_command`)
with no length limit. The result is a denial of service of the administrator's
workstation or MCP server. No cluster state changes.

**Shown by:** A 2,401-byte term of 200 brackets was refused only after 9 s, at a
peak heap of 2.4 GB. The 78-byte `a[0-1048575]!a[0-1048575],` repeated three
times was accepted after 9.6 s and 2.3 GB of allocations.

**Fix:** Multiply the running cross product as each dimension is parsed, and
pass the remaining budget into `parseRangeSet` so it refuses before allocating.
Bound the total work of an expression, for example the sum of term sizes.
Consider a lower cap and a length limit for MCP input.

### 4.8 The documentation contradicts itself and ClusterShell on padding

**Medium** · `nodeset/doc.go:31`, `doc/nodeset.md:47`

The package documentation of `nodeset`, the only public package, says that
`exe1` and `exe01` are two different hosts and that each element remembers its
width. The code, its tests, `doc/nodeset.md` and the manual all say the
opposite: `Parse("node1,node01")` yields one host, `node01`. `doc/nodeset.md`
and ADR 0002 also say these corner cases are decided the way ClusterShell
decides them, which is false. ClusterShell 1.10.1 keeps `exe1` and `exe01`
apart. It accepts `exe0[0,10]`, which clusterctl rejects on purpose, and it
rejects `exe[1-010]` and `exe[01-100]` with "padding length mismatch", which
clusterctl accepts. A library user who follows `doc.go`, or who compares output
with ClusterShell, gets merged or renamed hosts.

**Shown by:** A scratch test gave `Parse("exe1,exe01")` as `[exe01]` and
`Parse("exe[01-02],exe3")` as `exe[01-03]`. ClusterShell 1.10.1 gave `exe[1,01]`
(two hosts) and `exe[3,01-02]`.

**Fix:** Make `doc.go` describe the implemented behaviour. Remove the claim of
ClusterShell parity from `doc/nodeset.md` and ADR 0002, or list each divergence:
padding identity, adjacent numeric parts, padding-mismatch ranges, whitespace as
union and empty operands.

### 4.9 A bare `@group` falls through to other sources on any error

**Medium** · `internal/groups/groups.go:120`

`Resolver.Resolve` discards every error from the default source, including SSH
failures, time-outs and "returned no nodes". It then returns the first other
source, in alphabetical order, that answers. The docs describe a search for the
source that defines a group, and they tell users to write `@source:group` when
two sources could both answer. They do not say that a transient failure silently
switches to another source. With the shipped `examples/site`, a bare `@compute`
is the Slurm partition while the login node answers. When the login node does
not answer, it is every `exe` node from the static source, with exit 0 and
nothing on stderr. When every source fails, the final `no group source defines`
message hides the transport failure, and the command exits 2 instead of 3. A
destructive command still previews the resolved hosts, which limits the damage.

**Shown by:** With the example configuration, `node select @compute` printed
`exe[0001-0002]` when the recorder answered `sinfo`. When the recorder returned
`ssh: connect to host login01 port 22: Connection timed out`, it printed
`exe[0001-0010]`, exited 0 and wrote nothing to stderr.

**Fix:** Fall through only on a definite "group not defined" answer. Propagate
transport errors and empty results, and report which sources were tried and why
each failed.

### 4.10 One unreachable exec group source hides every membership

**Medium** · `internal/groups/groups.go:266`, `internal/cli/node.go:236`

`GroupsOf` returns `(nil, err)` as soon as an exec source's reverse lookup
fails. That throws away the memberships already found in the inventory and rack
sources, and the lookup never reaches `static`. `node describe` ignores the
error (`internal/cli/node.go:99`), so every `groups.*` row disappears with exit
0, and `node groups NODE` fails with exit 3. Without an argument, `node groups`
writes a failing source's error only into the `NODES` column, which is shown
only with `-o wide`. It also leaves the source out of JSON and YAML output,
prints nothing to stderr and exits 0. The troubleshooting guide sends
administrators to `node groups` to diagnose selection problems, and there the
failed source looks like a source with no groups.

**Shown by:** With the login host unresolvable, `node describe exe0007` printed
no `groups.*` rows and exited 0, and `node groups exe0007` exited 3. `node
groups` showed a blank `slurm` row and exited 0. Only `-o wide` showed `cannot
be listed: sinfo exited 255: ssh: Could not resolve hostname
login.hpc.example.org`.

**Fix:** Collect errors per source in `GroupsOf` and keep the results of the
other sources. Have `node describe` and `node groups` print what they have,
report the failed source on stderr and exit non-zero.

### 4.11 The state group `idle` includes drained nodes

**Medium** · `internal/slurm/slurm.go:147`

The `idle` group is passed to `sinfo` as `--states idle`, and the client does no
filtering afterwards. For a plain state, `sinfo` compares only the base state. A
drained node has base state IDLE with the DRAIN flag set, so it matches. The
`down` group includes `no_respond` and `power_down`. Both are flags, so they
match allocated and mixed nodes that are still running jobs, and idle nodes that
are only queued for power saving. The guide shows `slurm node nodeset idle`
leaving out the drained `exe0007`, and the same groups are offered to MCP
`query_slurm`. The node set looks plausible, so the preview does not reveal a
drained node pulled into `exec` or a reinstall.

**Shown by:** `slurm node nodeset idle` sent `sinfo ... --states idle`. Given a
reply with an idle `exe0001` and a drained `exe0007` ("failing DIMM, do not
touch"), it printed `exe[0001,0007]`.

**Fix:** After fetching, filter on the `%T` state string and keep only nodes
whose state, with suffixes stripped, is exactly the one asked for. Define `down`
from the reported state, not from flags that also match running nodes.

### 4.12 Without a `bmc` template the node's own name is its BMC, and `config init` does this for every node

**Medium** · `internal/naming/naming.go:90`

`Namer.BMC` returns the node's short name when no rule matches or when the first
matching rule has no `bmc` template. It does not continue to a later rule that
has one, and it gives no warning. The scaffold written by `config init` has a
single catch-all rule with only `fqdn`, and its comment says the service
processors keep the node's name. Redfish is tried first by default. The client
sends the site BMC account as HTTP Basic auth on every request and pins the
first certificate it sees without saying so. A site that adds `credentials.bmc`
to the scaffold therefore sends the BMC admin password to whatever the node's
short name resolves to, usually the node's own operating system. `bmc status` is
a read command, so an MCP agent can trigger this. Exploitation needs a
compromised node listening on port 443. It is not rated higher because both the
fallback and the silent pinning are documented.

**Shown by:** After `config init`, `node fqdn --bmc -n 'exe[01-02]'` printed
`exe[01-02]`, and `config validate` passed with a BMC credential added. `bmc
status -n localhost` then sent `user="admin" password="S3cretBMC"` to a TLS
listener on `127.0.0.1:443` and pinned its certificate, with no notice.

**Fix:** When no rule yields a BMC name, refuse, or continue to the next rule
that has a `bmc` template. Refuse a BMC name equal to the node's short name or
host name unless the configuration explicitly allows it. Report a first-contact
pin on stderr.

### 4.13 Naming is case-sensitive, drops the typed domain for BMCs, and loses the vendor profile for aliases

**Medium** · `internal/naming/naming.go:105`, `internal/naming/naming.go:86`,
`internal/app/bmc.go:53`

Rule matching and inventory lookup compare names exactly, and nothing lowercases
them. So `WLM01` misses the `[exe, sub, wlm, dbm]` rule, gets
`WLM01.example.org` and the BMC `WLM01.mgmt.example.org` from the catch-all
rule, and loses its inventory entry. `Namer.BMC` cuts a dotted name at the first
dot and applies the short name's rule without checking the typed domain. So
`login01.hpc.example.org` gets `login01.mgmt.example.org`, the BMC the rules
give `login01.example.org`, although the manual says a name with a domain names
a host precisely, and the preview shows only the typed name. `VendorProfile`
looks up the typed name, so for `exe0001.hpc.example.org`, `exe0001.` or
`EXE0001` the vendor's credential, transport order, `systemPath` and TLS
settings are dropped, while the command still reaches the node's real BMC. For
`exec` a capitalised name usually fails DNS or host key checking, and the
wrong-machine case for BMC actions needs one short name to exist in two domains.
The protected-host bypass through these spellings belongs to the safety gate and
is not counted here.

**Shown by:** `node fqdn -n WLM01` printed `WLM01.example.org`, and `bmc power
cycle --ipmi -y --force -n login01.hpc.example.org` ran `ipmipower --hostname
login01.mgmt.example.org --cycle`. A `vendor2` profile had its own account and
`order: [ipmi]`, yet `bmc power off -y --ipmi -n exe0001.hpc.example.org` sent
the site account `admin`, and the dry run printed `through Redfish`.

**Fix:** Lowercase node names on selection and when loading the inventory and
`protectedHosts`, or reject capitals. For a dotted name, derive the BMC only
when `Namer.FQDN` of the short name equals the typed name, and refuse otherwise.
Resolve aliases to the inventory name before any per-node lookup.

### 4.14 Inventory does not check that addresses and MACs are unique or well formed

**Medium** · `internal/inventory/inventory.go:50`

`inventory.New` checks only that fields describing one machine are set on
single-node entries. It accepts two nodes with the same `address`, `macs`, `cid`
or `bmcAddress`, and it accepts addresses that are not IP addresses, such as
`10.0.2.1/24` or `../../etc/x`. The inventory address is the first source of the
PXE and GRUB link names, and the reinstall confirmation does not show it. A
copied entry therefore reinstalls another machine. Values that are not IP
addresses also go straight into the link paths of `boot set`, `boot unset` (`rm
-f`) and `setBootPaths`.

**Shown by:** With `exe0002` given `address: 10.0.2.1`, copied from `exe0001`,
`provision reinstall -y -n exe0002` wrote `ln -sfn .../ipxe.net2
/srv/pxesrv/10.0.2.1`, and `config validate` printed `8 documents are valid`.

**Fix:** When the inventory is built, reject duplicate `address`, `macs`, `cid`
and `bmcAddress` values and addresses that do not parse as IP addresses. Name
the file and line of both entries in the error.

### 4.15 An explicit `rack` attribute is overwritten by the `rack` field

**Medium** · `internal/inventory/inventory.go:112`,
`internal/inventory/inventory.go:274`

`apply` copies an entry's attributes and then always writes `node.Rack` and
`node.Level` into them, including values inherited from an earlier entry. The
comment on `setAttr` promises to leave an attribute the entry set explicitly
alone, but the code does not. A refinement `{nodes: exe0001, attributes: {rack:
R05}}` is therefore reverted to the inherited `R02`, and `bmc power off -n
@rack:R02` still includes `exe0001`. The reverse also fails: a node whose rack
is set only as an attribute is in `@rack:R07` but missing from `Racks()` and
`InRack()`, so `node rack R07` and the `bmc` rack listing do not know it. In
addition, `InRack` compares case-insensitively while `@rack:` groups compare
exactly.

**Shown by:** A unit test with `exe[0001-0004] {rack: R02}` and `exe0001
{attributes: {rack: R05}}` left `exe0001` with the attribute `R02`, and
`WithAttribute(rack, R02)` returned `exe[0001-0004]`. `sub0001 {attributes:
{rack: R07}}` was in `WithAttribute(rack, R07)`, but `InRack("R07")` was empty.

**Fix:** Mirror the field only when the entry sets it, and never over an
attribute the entry set. Alternatively, reject an entry that sets both. Make
`Racks` and `InRack` read the same value the attribute source reads.

### Lower severity

- **4.16 Exec group sources have no timeout, and an empty role fails**
  (`internal/groups/groups.go:325`, `internal/groups/groups.go:309`). `exec`
  sends `transport.Request{Argv: command}` with no `Timeout`, so no `timeout -k`
  wrapper is added. A stalled login node then blocks every selection that names
  such a group, including dry runs. `ExecGroupSource.Role` is documented as
  "Empty runs them locally", but an empty role fails with `no host role was
  given`, and a bare `@group` hides that behind `no group source defines`. Pass
  the command timeout in the request, and either run locally when the role is
  empty or make the role required.
- **4.17 `node describe` and `node groups` use the typed name**
  (`internal/cli/node.go:91`). `node describe exe1` finds `exe0001` but shows
  host `exe1.hpc.example.org` and BMC `exe1.mgmt.hpc.example.org`, names that no
  acting command uses. It and `node groups exe1` also ask Slurm about `exe1`.
  When the node is known, use `node.Name` for the host, BMC and group lookups.
- **4.18 Selecting nodes costs quadratic time**
  (`internal/inventory/inventory.go:200`). `Resolve`, and `Lookup` for a name
  without an exact match, rebuild the whole inventory node set on every call,
  and `App.canonicalize` calls `Resolve` once per selected name. `node select
  @exe` on a 4,000-node inventory took 28.6 s. Build the canonical set and a
  name index once in `New`, and reuse them in `Lookup`, `Resolve` and `Select`.
- **4.19 The documented `@idle` and `@drained` groups do not exist**
  (`site/content/docs/getting-started/first-commands.md:53`). The
  getting-started and node-sets guides, `README.md`, the help for `-n` and
  `exec`, and the MCP server instructions use `@idle`, `@drained` and
  `@slurm:idle` as working node sets. No group source provides Slurm states, and
  the documented `slurm` source maps `$GROUP` to a partition, so `node select
  '@compute!@drained'` fails with `no group source defines "drained"` and
  `@slurm:idle` runs `sinfo -p idle`. Add a state source backed by
  `slurm.NodeSet`, or change the examples and instructions to use partitions and
  `slurm node nodeset`.
- **4.20 The fuzz oracle cannot detect a renamed host**
  (`nodeset/fuzz_test.go:53`). `FuzzParseFold` checks the round trip with `Len`
  and `Contains`, and `Contains` ignores padding, so a fold that renamed `exe3`
  to `exe03` would pass. No test covers an operator with a missing operand
  (4.1), a name with a leading dash, or mixed padding widths. Compare
  `first.Expand()` with `second.Expand()` exactly, add table tests for those
  inputs, and add a ClusterShell differential corpus.

## 5. DHCP, PXE and reinstall

This section covers how clusterctl resolves a node's address from `dhcpd.conf`,
how it writes PXE and GRUB links, and how `provision reinstall` orders its
changes. On these paths, a parsing or ordering fault arms the wrong machine for
a reinstall, or leaves the right one armed without saying so.

### 5.1 A comment in `dhcpd.conf` that names a node links another node's address

**High** · `internal/dhcp/dhcp.go:180`

`matchesNode` treats a host declaration as the node's own when any word in the
comment lines directly above it equals the node name. `Parse` sorts declarations
by name and `nodeAddress` returns the first match that has an address. So a
neighbour's declaration that sorts earlier beats the node's own `host exe0004`
declaration when its comment lists the node, as in `# chassis C07: exe0003
exe0004` or `# rack R02: exe0001 to exe0010`. This applies to every node without
an inventory address, which in the example site is every node except `wlm01`,
`dbm01` and `exe0001`. `boot set`, `boot grub set` and `provision reinstall`
then write the install link for the neighbour, which was never selected or
confirmed and reinstalls at its next network boot, while the selected node does
not. The confirmation names the node and the boot path but not the address, and
the package comment (`internal/dhcp/dhcp.go:6-9`) promises that the parser does
not report a neighbour's address.

**Shown by:** A harness test served `# chassis C07: exe0003 exe0004` above
multi-line declarations for `exe0003` (10.0.2.3) and `exe0004` (10.0.2.4). `boot
set -n exe0004`, confirmed with `y`, printed `exe0004  10.0.2.3` and sent `ln
-sfn /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2 /srv/pxesrv/10.0.2.3` to the
install host.

**Fix:** Stop using a comment to give a declaration's address or MACs to a node,
and use comment matches for display only. Take the boot address from the
declaration named exactly after the node or its FQDN, and show the resolved
address in the confirmation.

### 5.2 The DHCP parser merges or drops declarations on common layouts

**High** · `internal/dhcp/dhcp.go:91`, `internal/dhcp/dhcp.go:84`,
`internal/dhcp/dhcp.go:120`

`Parse` reads the file line by line and closes a declaration only on a line that
starts with `}`. dhcpd accepts a last statement that shares its line with the
brace (`fixed-address 10.0.2.3; }`), but the parser leaves such a declaration
open. The next `host exe0004 {` becomes an option, `exe0004`'s MAC is appended
to `exe0003`'s, its `fixed-address` replaces `exe0003`'s, and `exe0004`
disappears without an error. A hand-edited file that mixes the two styles is
enough. Inside a `group` or `subnet` block the enclosing brace closes the merged
declaration, so even a file written wholly in the trailing-brace style parses
without error, and the merged address keeps a trailing `;` (`10.0.2.4;`). The
line-by-line approach loses data elsewhere too:

- A one-line declaration is closed before its statements are read.
- Statements after `{` on the `host` line are dropped.
- Two statements on one line become one, and the MAC keeps a trailing `;`.
- A `#` inside a quoted string cuts the value short.
- `include` is not followed.

Most of these fail safe with `no address is known`, although `dhcp hosts` exits
0 with empty columns. The merge does not fail safe: `boot set` or `provision
reinstall` of `exe0003` links `exe0004`'s address.

**Shown by:** `exe0003` closed on `fixed-address 10.0.2.3; }` and `exe0004`
closed on its own line. With that file, `boot set -n exe0003` printed `exe0003
10.0.2.4` and linked `/srv/pxesrv/10.0.2.4`, and `dhcp hosts -n exe[0003-0004]`
showed `exe0003` with both MACs and `exe0004 not in the DHCP configuration`.

**Fix:** Split the file into tokens (quoted strings, comments, `;`, `{`, `}`)
instead of reading lines, and follow `include`. Treat a `host` statement inside
a host declaration, and any construct the parser does not understand, as a parse
error.

### 5.3 A PXE link is written for another machine's address when several declarations match

**High** · `internal/cli/boot.go:52`, `internal/dhcp/dhcp.go:170`

`nodeAddress` returns the address of the first declaration that `Lookup`
returns. `Lookup` also accepts names that begin with the node name followed by
`-`, `.` or `_`, and FQDNs whose short name is the node. Declarations are sorted
by name and `-` sorts before `.`, so `exe0005-bmc.mgmt.hpc.example.org` beats
`exe0005.hpc.example.org`. An interface declaration in a subdomain that sorts
first wins in the same way, and nothing checks whether more than one candidate
carries an address. With these rules alone, the winner is usually the same
machine's BMC or another of its interfaces. `provision reinstall` then resets
the node, reports every step `ok`, and nothing is installed. Another machine is
armed when the extra match comes from a comment (5.1), or from a stale
`exe0005-old` declaration whose address now belongs to another node. This
requires a node with no inventory address and declarations written over several
lines, because one-line declarations carry no address (5.2).

**Shown by:** `host exe0005.hpc.example.org` was at 10.0.2.5 and `host
exe0005-bmc.mgmt.hpc.example.org` at 10.9.2.5. `boot set -y -n exe0005` printed
`exe0005  10.9.2.5` and linked `/srv/pxesrv/10.9.2.5`. With `# rack R02: exe0001
to exe0010` above `host exe0001`, `provision reinstall -y --no-reset -n exe0010`
reported `configure the network boot ok` and linked `/srv/pxesrv/10.0.2.1`,
which is `exe0001`'s address.

**Fix:** Take the boot address only from a declaration named exactly after the
node or its FQDN, and refuse when more than one such declaration carries an
address. Show suffixed names as extra interfaces but never use them as the boot
address, and check that no two nodes in the set resolve to the same address.

### 5.4 Reinstall changes hosts before everything is resolved

**High** · `internal/cli/provision.go:508`, `internal/cli/provision.go:593`,
`internal/cli/provision.go:613`

`doc/safety.md`, R49 in `doc/requirements.md` and the comment above the
resolution loop all promise that a reinstall resolves everything before it
changes anything. In the code, only the boot paths are resolved before the gate,
and each address is looked up and then discarded. After the gate the command
does these things in order:

1. It rewrites the host key file.
2. It resolves the PXE role and every address again, and fetches `dhcpd.conf`
   again once `services.dhcp.cacheTtl` (2 minutes by default) has expired.
3. Only then, in `forEachBMCError`, does it resolve each BMC host name and
   credential. It always uses Redfish, whatever the vendor `order` says.

A missing `BMC_PASSWORD`, a sops or age decryption failure, or a credential
prompt without a terminal therefore turns up only after the host keys are gone
and the install links are written. The nodes keep running their old system.
clusterctl can no longer reach them over ssh, because host key checking is
strict. Each node carries an install link that fires at its next network boot.

**Shown by:** With `BMC_PASSWORD` unset, `provision reinstall -n exe0001 -y`
printed `forget the host keys ok`, `configure the network boot ok` and `boot
from the network once failed: exe0001: credential "bmc" reads BMC_PASSWORD,
which is not set`. It exited 1 and left the known-hosts file without `exe0001`.
With `--set services.dhcp.cacheTtl=1ns` and a DHCP host that timed out on the
second read, the command fetched `dhcpd.conf` twice and failed at `configure the
network boot` after it had deleted the host keys.

**Fix:** Before the gate, resolve for each node the address, the PXE role, the
BMC host, the credential and the BMC transport. Keep them in the plan and pass
the plan to `setBootPaths` instead of resolving again. Refuse nodes whose
transport order does not start with `redfish`, and touch the host key file only
after all resolution has succeeded.

### 5.5 `boot unset` and `boot status` ignore the persistent link

**High** · `internal/cli/boot.go:276`, `internal/cli/boot.go:127`

`boot set --persistent` links `<root>/<address><staticSuffix>`. `boot unset`
removes only `<root>/<address>` and then prints `removed the boot configuration
of exe0001`. `boot status` with a node set looks up only the plain link, so a
node whose only link is the persistent one shows `none`; only `boot status`
without a node set lists the raw `.static` entry. `provision reinstall` and
one-shot `boot set` do not look for an existing persistent link either. The
`boot` help and `doc/safety.md` name the persistent path as what leaves a
machine reinstalling on every reboot, yet here the tool reports the path gone
while it remains. This needs `services.pxesrv.staticSuffix` to be set. The
example site sets none, and without one `--persistent` writes the one-shot link
(5.8).

**Shown by:** With `--set services.pxesrv.staticSuffix=.static`, `boot set -n
exe0001 --persistent -y` linked `/srv/pxesrv/10.0.2.1.static`. `boot unset -n
exe0001 -y` then sent only `rm -f /srv/pxesrv/10.0.2.1` and exited 0, and `boot
status -n exe0001` printed `exe0001  10.0.2.1  none`.

**Fix:** When a suffix is set, make `boot unset` remove both `<root>/<address>`
and `<root>/<address><staticSuffix>`, and make `boot status` report both links
for each node. Have `boot set` and `provision reinstall` warn about, or refuse,
an existing persistent link.

### 5.6 A failed reinstall leaves nodes armed, in random order, with no report

**Medium** · `internal/cli/provision.go:557`, `internal/cli/provision.go:592`

`provision reinstall` writes the install links, then sets a one-time `Pxe` boot
override on every BMC. It resets the machines only when every override
succeeded. If one BMC fails, for example with a timeout or a rejected `PATCH`,
`forEachBMCError` still sets the override on all the others. `record` then
returns, and no node is reset or rolled back. Those nodes keep a pending `Once`
override and a live install link, and they reinstall at their next reboot,
whenever that comes, with no further confirmation. The step table shows
`configure the network boot ok` and names the failed nodes, so the armed set can
be worked out, but nothing says the nodes are armed or how to disarm them. When
the link script itself fails part way, the armed subset is random.
`setBootPaths` ranges over a map, so the order of the `ln` lines changes from
run to run, and so does which nodes already got a link. The table then says only
`failed: install (...): command exited 1`.

**Shown by:** Fake BMCs answered for `exe0001` and `exe0003`, and none for
`exe0002`. `provision reinstall -n exe[0001-0003] -y` failed at `boot from the
network once` with `exe0002: ... connection refused`. Both reachable BMCs
recorded the `Once`/`Pxe` `PATCH` and received no reset and no cleanup. In a
second test, the link for `10.0.2.6` could not be written, and four runs of
`provision reinstall -y --keep-host-keys -n exe[1-10]` armed four different
subsets.

**Fix:** When a step fails after the first change, clear the overrides and
remove the links of the nodes that were not reset. At the least, print the node
set left armed and the `bmc boot unset` and `boot unset` commands that disarm
it. Write the links in node-set order and report the outcome for each node.

### 5.7 `boot set` and `boot unset` stop part way through one script and report nothing per node

**Medium** · `internal/cli/boot.go:213`, `internal/cli/boot.go:270`

Both commands send one `set -eu` script, with an `ln -sfn` or `rm -f` line for
each node. One line can fail, for example on a full or read-only file system, on
a directory where a link should be, or when the remote `timeout` kills the shell
on a hung mount. The nodes before that line are then changed and the nodes after
it are not. Neither command reports anything per node on failure, in table or
JSON output: `boot set` returns before it builds its table and `boot unset`
before it prints its note. The only message names the install host, with exit 3,
which the documentation reserves for a host that could not be reached. Running
the command again stops at the same line, so after a reinstall, `boot unset`
leaves the later nodes armed to reinstall at their next network boot.

**Shown by:** In a scratch PXE root, `10.0.2.6` was a directory. `boot unset -y
-n exe[1-10]` exited 3 with nothing on stdout and only `install
(installer.hpc.example.org): command exited 1`, and it left the links of
`exe0007` to `exe0010` in place. `boot set` against the same kind of fault
linked `10.0.2.1` to `10.0.2.5` only, with the same output.

**Fix:** Drop `set -e` and have each line report its own result (`ln -sfn ... &&
echo "ok $addr" || echo "fail $addr"`). Turn that output into a per-node table
or JSON with an `unchanged` state, and exit 1 naming the failed and untouched
nodes. Alternatively, send one request per node through the executor.

### 5.8 `--persistent` writes the one-shot link when `staticSuffix` is unset; `boot grub set` is always permanent

**Medium** · `internal/cli/boot.go:217`, `internal/cli/boot.go:188`,
`internal/cli/boot.go:449`

`services.pxesrv.staticSuffix` has no default. Without it, `boot set
--persistent` appends an empty suffix and writes exactly the one-shot link,
while the preview says `persistently`. The `static` field of a boot path rule is
returned by `inventory.BootPath` and discarded at both call sites
(`boot.go:188`, `provision.go:501`). A rule marked `static: true` therefore
writes a one-shot link even when a suffix is set, and a node meant to boot from
the network every time finds no path on its second boot. `boot grub set` has the
opposite fault: its `grub.cfg-<HEX>` link is permanent, with no once mode, no
mention of persistence in the preview and no `boot grub unset`. `doc/safety.md`
and the `boot` help say a boot path applies to one request by default. `boot
grub unset` exits 0 and prints the group help, and `boot unset` does not touch
the GRUB link, so the link can be removed only through `boot shell`.

**Shown by:** With the example configuration, `boot set -n exe0001 --persistent
-y` and `boot set -n exe0001 -y` sent the same `ln -sfn ...
/srv/pxesrv/10.0.2.1`, and `--dry-run` printed `persistently`. `boot grub set
exe0001 /srv/tftp/grub/1.0/grub.cfg.install-exec --dry-run` printed only the
link and its target, and `boot grub unset exe0001` recorded no call and exited
0.

**Fix:** Refuse `--persistent` and `static: true` rules when
`services.pxesrv.staticSuffix` is empty, and honour the rule's `static` flag in
`boot set`. Add `boot grub unset`, and state in the `boot grub set` preview that
the link persists.

### 5.9 Boot previews hide what will be installed

**Medium** · `internal/cli/boot.go:204`, `internal/cli/provision.go:516`

The `boot set` confirmation shows only the first node's boot path, although each
node's path is resolved separately. When the set spans several installations,
the administrator confirms one path and other paths are written. The `provision
reinstall` preview shows no boot path at all, only `everything on these machines
is lost`. Neither `--boot-path` nor the paths from the rules are checked on the
PXE host, so a mistyped path writes dangling links and the machines are reset
into a failed network boot. The misleading `boot set` preview is the clear
defect; the missing path check is an absent safeguard rather than a broken
promise.

**Shown by:** `--force --dry-run boot set -n dbm01,exe0001` previewed only
`/srv/pxesrv/boot/cluster/1.0/dbm01/ipxe.net2`, and the real run linked
`exe0001` to `.../exe/ipxe.net2`. `--dry-run provision reinstall -n exe0001
--boot-path /srv/pxesrv/boot/cluster/1.O/exe/ipxe.net2`, with a letter O in
place of the zero, printed only `Would reinstall 1 host: exe0001` and the
warning, and raised no error about the path.

**Fix:** In both previews, list each distinct boot path with the nodes it
applies to. Before the gate, check each distinct path on the PXE host (`test
-f`) and refuse any that is missing.

### 5.10 The cached `dhcpd.conf` is keyed by role name only

**Medium** · `internal/app/remote.go:66`

`RemoteFile` caches under `<cache dir>/remote/<role>-<hash of role and path>`.
The key leaves out the host the role resolves to, the site and the context. The
cache directory is per user, and every `--config`, `CLUSTERCTL_CONFIG`, context
and the MCP server share it. Suppose two sites both name the role `dhcp` and use
the same path. Then, within `services.dhcp.cacheTtl` (2 minutes by default), a
command for site B reads site A's `dhcpd.conf`. `boot set`, `boot status`,
`provision reinstall` and `fabric` use that file for every node without an
inventory address, and where the two address plans overlap, a different machine
in B is armed.

**Shown by:** First `dhcp hosts` ran against the example site. Then `boot set -y
-n exe0002` ran against a copy whose DHCP host was `dhcp02.other.org`. The
second command never contacted that host. It printed `exe0002 10.0.2.2`, site
A's address (B's is `10.99.0.2`), and linked `/srv/pxesrv/10.0.2.2` on B's PXE
host. Both sites used the single cache file `dhcp-f36f90275f223d30`.

**Fix:** Include the resolved target (`user@host`) and the site and cluster
identity in the cache key.

### 5.11 `cinc run` fails whenever `cinc config` was used without `--run-list`

**Medium** · `internal/cli/provision.go:421`

`cinc config` documents the run list as optional and writes `CHEF_RUN_LIST` only
when `-R` is given. It also replaces the whole file each time, so running it
without `-R` removes a run list written earlier. Without `-R`, `cinc run` always
generates `--override-runlist "$CHEF_RUN_LIST"` under `set -eu`, which aborts on
every node with `CHEF_RUN_LIST: unbound variable`. Nothing is changed and the
failure is visible, but `cinc run` fails after every use of the documented short
form of `cinc config`.

**Shown by:** `cinc run -n exe0001 -y` sent `set -eu`, `. /etc/cinc/solo` and
`exec cinc-solo ... --override-runlist "$CHEF_RUN_LIST"`. Sourcing a file that
holds only `CHEF_RECIPE_URL=...`, as `cinc config` writes it without `-R`,
printed `CHEF_RUN_LIST: unbound variable` and exited 1.

**Fix:** Pass the override only when the file sets it, for example with
`${CHEF_RUN_LIST:+--override-runlist "$CHEF_RUN_LIST"}`, or make `--run-list`
required in `cinc config`.

### Lower severity

- **5.12 Reinstall exit codes, help text and test coverage**
  (`internal/cli/provision.go:623`, `internal/cli/provision.go:570`,
  `internal/cli/cli_test.go:272`). `forEachBMCError` wraps every per-node
  failure as `TargetFailed`, so a missing BMC credential (exit 2) and an
  unreachable BMC (exit 3) both exit 1. The fix is to keep the most specific
  code and to make the Redfish client tag connection failures as `Transport`,
  which it does not do today. The `provision status` help promises the
  configured boot path, which the table does not show, and `provision reinstall
  --no-reset` still ends with `<set> is reinstalling`; add the column or correct
  the help, and change the caption when nothing was reset. R48 and R49 are
  marked met, yet the only reinstall test is a dry run of `exe0001`, which has
  an inventory address; add harness tests that assert nothing was changed for an
  unknown node, a credential or BMC failure, and DHCP-derived addresses.

## 6. ssh configuration, host keys, tunnels and copy

This section covers the ssh configuration clusterctl generates, the host key
file that configuration checks against, and the `tunnel`, `hostkey`, `copy` and
`login` commands. Every connection clusterctl makes depends on this code. A
defect here can weaken host key checking, or send a command to the wrong host or
account.

### 6.1 Host keys are not checked against one file

**High** · `internal/transport/sshconfig.go:109`

The `Host *` block sets only `UserKnownHostsFile` and `StrictHostKeyChecking`.
It never sets `GlobalKnownHostsFile`, `KnownHostsCommand` or `VerifyHostKeyDNS`.
OpenSSH therefore still reads its default global files,
`/etc/ssh/ssh_known_hosts` and `ssh_known_hosts2`. It also reads any trust
source that the included `/etc/ssh/ssh_config` and `ssh_config.d/*.conf` add,
such as the `GlobalKnownHostsFile` and `sss_ssh_knownhosts` command of a
FreeIPA/sssd client. OpenSSH accepts a host when any loaded source holds a
matching key, so a key vouched for by a global source wins even when the site
file holds a different key. On a workstation with a populated global file or an
IPA enrolment, `hostkey verify` reports a key as changed while `exec`, `secrets
push` and the other commands connect without a warning and send secrets to that
key. This breaks R03 (`doc/requirements.md:20`, marked met),
`doc/transport.md:145-146` and `site/content/docs/guides/access.md:16`.

**Shown by:** The test used a scratch `x/crypto/ssh` server, real OpenSSH 9.6p1,
and a different key for the server in the site file. `transport.Client.Run`
failed with "Host key verification failed". Adding an `ssh.include` file that
sets `GlobalKnownHostsFile` to a file holding the real key made the same run
exit 0 and print the server's output. `ssh -G -F` on the file generated from
`examples/site` printed `globalknownhostsfile /etc/ssh/ssh_known_hosts
/etc/ssh/ssh_known_hosts2`.

**Fix:** In `Host *`, write `GlobalKnownHostsFile /dev/null` (or the site file),
`KnownHostsCommand none`, `VerifyHostKeyDNS no` and `UpdateHostKeys no`. Precede
`KnownHostsCommand` with `IgnoreUnknown KnownHostsCommand`, because OpenSSH 8.0
aborts on unknown keywords. Refuse to generate a configuration when
`ssh.knownHostsFile` is unset, because ssh then falls back to
`~/.ssh/known_hosts`.

### 6.2 All contexts share one generated ssh_config

**High** · `internal/transport/sshconfig.go:198`

The configuration is always written to `<state dir>/ssh_config`. The state
directory is per user, not per context, configuration or process. Each process
writes the file once and passes the same path with `-F` to every later ssh,
which re-reads the file each time it starts. Another clusterctl process of the
same user can overwrite the file in between, even with `--dry-run`: another
context, `--config` tree or `--set` is enough. The first process's remaining
connections then use the other run's `UserKnownHostsFile`,
`StrictHostKeyChecking` and role blocks. If the other run sets
`ssh.strictHostKeyChecking: false`, a documented bring-up setting, the result is
a host key downgrade to `accept-new` against the wrong file. Otherwise it is
spurious host key failures or a wrong `User` or `ProxyJump`. The long-running
MCP server, which builds a new App for every call, and fan-outs that run for
minutes make such overlap likely.

**Shown by:** `--context cluster1 login --dry-run install -- true` wrote
`StrictHostKeyChecking yes`. Then `--context cluster2 --set
ssh.strictHostKeyChecking=false --set ssh.knownHostsFile=/other/site/known_hosts
login --dry-run install -- true` printed the same `-F` path, and the file then
read `UserKnownHostsFile /other/site/known_hosts` and `StrictHostKeyChecking
accept-new`. The strict site's file did not hold the key of a real OpenSSH 9.6
test server. `ssh -F` with the shared file refused the server before the second
write and connected to it after.

**Fix:** Write the configuration to a file named by a hash of its content, or by
context and process id, and create it exclusively. Remove it when the process
ends.

### 6.3 sshuttle tunnels ignore the generated ssh configuration

**High** · `internal/tunnel/tunnel.go:86`

`Manager.Args` builds `sshuttle --daemon --pidfile F --remote [user@]host ...`
with no `--ssh-cmd`. sshuttle therefore runs plain `ssh` with the
administrator's own `~/.ssh/config` and `~/.ssh/known_hosts`. None of the
generated settings apply: not the site host key file, not `StrictHostKeyChecking
yes`, not the role's `proxyJump` or `options`, and not the context's default
user. The role's own `user` is applied. The key is still checked against the
administrator's personal known_hosts, so this is not a full bypass. But under a
personal `StrictHostKeyChecking accept-new` or `no`, a spoofed gateway is
accepted even though the site file pins a different key. The routed network, and
DNS with `dns: true`, then goes through that gateway. This contradicts
`site/content/docs/guides/access.md:16` and `:49`.

**Shown by:** Reading `tunnel.go:56-114` shows that the argument vector never
contains `-F` or `--ssh-cmd`. A grep for `ssh-cmd` and `ConfigPath` in
`internal/tunnel` and `internal/cli/tunnel.go` finds nothing. The dry run
documented in `access.md:114-115` shows the same argument vector.

**Fix:** Pass `--ssh-cmd 'ssh -F <state>/ssh_config'`, with the path from
`a.SSH.ConfigPath()` quoted for sshuttle's shell-style split. The tunnel then
uses the same trust anchor, jump hosts and user as every other connection.

### 6.4 A newline in a configuration value injects ssh_config lines

**Medium** · `internal/transport/sshconfig.go:67`

`writeRoleBlock` and `writeGlobal` write values with `fmt.Fprintf` and never
check them for newlines. This covers role descriptions, hosts, users, jump
targets, option keys and values, `sendEnv`, the host key file path and the
includes. The schema has no pattern for `description`. A plain YAML `|`
description, which `config validate` accepts, ends the comment line. Its second
line then stands as a bare directive inside the preceding role's `Host` block,
or at global scope when the role sorts first. ssh rejects the whole file, so
every ssh and scp call clusterctl makes fails. The failures are reported as
failed hosts, not as a configuration error. An injected `StrictHostKeyChecking
no` would also take effect, but a site author can already set any keyword
through `options`, so the added risk is small.

**Shown by:** The `mgmt` role's description in a copy of `examples/site` was
made two lines. `config validate` printed "7 documents are valid". `exec -n exe1
-- uptime` failed with ".../ssh_config: terminating, 1 bad configuration
options", exit 1.

**Fix:** Reject control characters in every value written to the file during
validation, and write a description as one comment line per source line. Check
that hosts, users and option keys are single tokens, and double-quote values
that may contain spaces.

### 6.5 `ControlMaster` is not pinned off

**Medium** · `internal/transport/sshconfig.go:54`,
`internal/transport/sshconfig.go:170`

The `Host *` block never writes `ControlMaster no` or `ControlPath none`, and
the includes come after it. A `ControlMaster` set in an included file therefore
applies to every compute node. A connection through an existing master performs
no host key check against the site file, so a master opened by the
administrator's own ssh with its own known_hosts is reused as is. The
preconditions are narrow. The default include, `/etc/ssh/ssh_config`, does not
enable multiplexing on stock RHEL or Ubuntu, so a site must add an include that
does, and the administrator's own ssh must have accepted a bad key. Role masters
also share `cm-%C` in the per-user state directory whatever the host key file or
strictness. A master opened by a run with `strictHostKeyChecking: false`, or by
another context, is reused by later runs for the whole `controlPersist` period.
Once such an include exists, R09 ("never per node") and R03 are not enforced.

**Shown by:** A scratch test opened a master with plain `ssh -fN` trusting the
real key. It then ran `transport.Client.Run` with a site file holding a
different key. Without an include the run exited 255 with "Host key verification
failed". With an `ssh.include` file setting `ControlMaster auto` and the same
`ControlPath`, it exited 0 with the server's output.

**Fix:** Write `ControlMaster no` and `ControlPath none` in `Host *`. Role
blocks come first, so roles that opt in still multiplex. Put a hash of the host
key file path and the strictness setting into the role `ControlPath`, or use a
per-context state directory.

### 6.6 The user's own ssh configuration is not in effect, although the docs say it is

**Medium** · `internal/config/defaults.yaml:38`

Every connection runs `ssh -F <generated>`, and with `-F` ssh reads neither
`~/.ssh/config` nor the default system file. The generated file includes only
`ssh.include`, which defaults to `/etc/ssh/ssh_config`. `config init` and the
example site add nothing. Several places promise the opposite: R06
(`doc/requirements.md:23`), `doc/transport.md:68-69`,
`site/content/docs/guides/access.md:57-59`, the package comment in
`transport.go`, and the `config init` scaffold (`config.yaml.tmpl:16-17`: "ssh
decides: your own ssh_config"). An administrator who relies on `User`,
`IdentityFile` or `ProxyJump` in `~/.ssh/config` logs in as the local user name,
with the default identities and no jump host. The connection then fails, or
authenticates as a different account.

**Shown by:** `strace` of `ssh -G -F <generated from examples/site>
exe0007.hpc.example.org` showed ssh opening only the generated file,
`/etc/ssh/ssh_config` and `ssh_config.d`, never `~/.ssh/config`. The same
command without `-F` opened `/root/.ssh/config` first.

**Fix:** Include `~/.ssh/config` by default, with `ControlMaster` pinned off in
`Host *` (6.5). Otherwise correct R06, the manual and the scaffold comment to
say that only the system file is included.

### 6.7 A configuration path containing a space breaks the host key files

**Medium** · `internal/transport/sshconfig.go:109`

The host key file path is resolved against the Site directory and written
unquoted as `UserKnownHostsFile`, and OpenSSH splits the value on whitespace.
ADR 0017 makes `~/Library/Application Support/clusterctl` the default `config
init` target on macOS. There the value becomes two entries:
`.../Library/Application` and the relative `Support/clusterctl/ssh-known-hosts`,
which is resolved against clusterctl's working directory. ssh never reads the
real file, so on a default macOS setup every connection fails host key
verification. If the working directory holds a
`Support/clusterctl/ssh-known-hosts` file, the keys in it are trusted instead.
That needs someone able to plant the file there.

**Shown by:** After `config init '<scratch>/Library/Application
Support/clusterctl'`, the generated file held the path unquoted. `ssh -v` logged
"fopen .../Library/Application: No such file or directory", then "fopen
Support/clusterctl/ssh-known-hosts", then "Host key verification failed". With
the path quoted by hand, ssh found the key and connected. Run from a directory
holding a planted `Support/clusterctl/ssh-known-hosts`, ssh authenticated the
server against the planted file.

**Fix:** Double-quote every path written into the file (`UserKnownHostsFile`,
`ControlPath`, `Include`), or refuse paths that contain whitespace or quote
characters.

### 6.8 A node whose host name equals a role host logs in with the role's account

**Medium** · `internal/transport/sshconfig.go:73`

Role blocks match the real host name and come before `Host *`, so their `User`
line applies to every connection to that host. A node target carries a user only
when the context sets one: `app.Node` takes `defaultUser`, and `transport.Args`
writes `user@` only when there is a user. `config init` without `--user` sets no
context user. In that case a node whose FQDN equals a role host logs in with the
role's account. In `examples/site`, the `wlm` and `db` roles are `user: root` on
the inventory nodes `wlm01` and `dbm01`. So `exec -n 'wlm01,exe[0001-0010]'`
runs as root on the Slurm controller and as the local user everywhere else.
`copy` and MCP read commands do the same. This contradicts the scaffold comment
that without a user "ssh decides: your own ssh_config, or your local user name".

**Shown by:** The test used a copy of `examples/site` with the context user
removed and the `wlm` role user set to `slurmadm`. `login --dry-run wlm01 -- id`
printed no `user@`. `ssh -G` with the generated file resolved `user slurmadm`
for `wlm01.hpc.example.org` and the local account for `exe0001.hpc.example.org`.
A real `exec --force` run resolved the same accounts.

**Fix:** Always pass an explicit user for node targets: the context user, or the
local user name when none is set. Alternatively, reject or warn in `config
validate` when a role host is also a node FQDN.

### 6.9 A `proxyJump` cycle starts an endless chain of ssh processes

**Medium** · `internal/transport/sshconfig.go:84`

Each role's `proxyJump` is resolved on its own through `resolveJump` (line 175),
and nothing checks for cycles. `config validate` accepts `mgmt: {proxyJump:
dhcp}` together with `dhcp: {proxyJump: mgmt}`. OpenSSH detects only a host that
jumps through itself. With a two-role cycle, each ssh starts an `ssh -W` for the
other host before any network activity, and the chain grows without bound.
clusterctl gives up after `ConnectTimeout` with exit 3, but the chain outlives
it, because only the top-level ssh is killed. Any command on either role
triggers it, including IPMI through `mgmt` and MCP read commands. The chain
floods the workstation with ssh processes until it reaches the process limit or
someone kills them. Nothing happens on the cluster.

**Shown by:** With `proxyJump: dhcp` added to `mgmt` in the example site, `login
-T mgmt -- true` exited 3 after 15 s. 1600 ssh processes were still running
afterwards, and removing them took 135 rounds of `pkill -STOP` and `pkill
-KILL`.

**Fix:** Resolve `proxyJump` chains when the configuration is loaded and reject
cycles with a usage error in `config validate` and `app.New`. This includes
chains through literal hosts that are role hosts.

### 6.10 `hostkey scan` cannot collect `ssh-rsa` keys and dials past `ProxyJump`

**Medium** · `internal/hostkeys/hostkeys.go:31`, `internal/cli/hostkey.go:57`

The scanner offers only `ssh-ed25519`, `ecdsa-sha2-nistp256` and `rsa-sha2-256`.
A server that signs only with SHA-1 `ssh-rsa` (OpenSSH before 7.2) therefore has
no algorithm in common with it. That is the kind of host `legacyAlgorithms`
exists for. The scanner also dials every host directly from the workstation and
ignores the role's `proxyJump`. The example's `dhcp` role sits behind `mgmt` and
offers only `ssh-rsa`, so it cannot be scanned at all. With strict checking,
such hosts stay unusable until someone collects the key with `ssh-keyscan` or
relaxes strictness. R04 promises collection without external tools, and
`troubleshooting.md` recommends `hostkey refresh` for this case. The failure is
closed, not unsafe.

**Shown by:** A scratch test against a server restricted to `ssh-rsa` failed
with "no common algorithm for host key; we offered: ["rsa-sha2-256"], peer
offered: ["ssh-rsa"]". Ed25519 and unrestricted RSA servers were collected.
`hostkey scan -n dhcp01 --timeout 2s` failed on a direct DNS lookup of
`dhcp01.example.org`.

**Fix:** Fall back to `ssh-rsa` when negotiation fails, since the key is the
same RSA key. Scan role hosts through their jump host, for example with `ssh -W`
through the transport.

### 6.11 `tunnel stop` trusts a stale pid file and can kill an unrelated process

**Medium** · `internal/tunnel/tunnel.go:187`, `internal/tunnel/tunnel.go:232`

`running()` only sends signal 0 to the recorded PID. It never checks that the
process is sshuttle for this profile. The pid file lives in the persistent
`$XDG_STATE_HOME/clusterctl/tunnels`, so it outlives `kill -9`, an OOM kill or a
power loss, and the PID can later belong to another process of the same user.
`tunnel status` then shows the tunnel up, `tunnel start` refuses with "already
running", and `tunnel stop` sends SIGTERM to the unrelated process. This breaks
the promise in the help text and `site/content/docs/guides/access.md:110-111`
that a leftover pid file is not reported as running. `Stop` also never checks
that the name is a configured profile. `tunnel stop ../../x` reads, signals and
deletes an `x.pid` outside the tunnels directory. That part has little impact,
because the administrator types the name.

**Shown by:** A scratch test wrote the PID of a `sleep 30` into the `ipmi` pid
file. `Status` reported the tunnel running, `Start` refused, and `Stop`
terminated the sleep. `Stop("../../victim")` deleted a `victim.pid` two levels
above the tunnels directory.

**Fix:** Check the process identity: `/proc/PID/cmdline` must name sshuttle with
this `--pidfile`, or the process start time must match the pid file's mtime.
Reject names in `Stop` that are not configured profiles.

### Lower severity

- **6.12 Smaller ssh_config generator defects**
  (`internal/transport/sshconfig.go:100`). The generator writes several values
  unchecked and silently drops others:
  - `internal/transport/sshconfig.go:45`: when roles share a host, only the
    alphabetically first role gets a block. The others' `proxyJump`,
    `controlMaster`, `legacyAlgorithms` and `options` are dropped. Merge them,
    or reject roles with conflicting settings.
  - `internal/transport/sshconfig.go:100`: role `options` come before `Host *`.
    `StrictHostKeyChecking: "no"` or `UserKnownHostsFile: /dev/null` there
    overrides the site trust settings for that host and for a node of the same
    name. A `Match` or `Host` key opens a new block, so the options after it can
    apply to every host. This needs a site author to write those keys. Refuse
    `Host`, `Match`, `Include` and the trust keywords in role and global
    options.
  - `internal/transport/sshconfig.go:175`: `host` is written as a `Host` pattern
    without checks, so a wildcard gives every node connection the role's
    `ForwardAgent`, user and options. `proxyJump` is looked up as a single role
    name, so `mgmt,install`, `root@mgmt` and misspelt role names become literal
    host names. Validate `host` as a host name and resolve each `proxyJump`
    element.
  - `internal/cli/doctor.go:91`: one misspelt option keyword breaks every
    connection, yet `config validate` and `doctor` report success. `classify`
    keeps only the last stderr line ("terminating, 1 bad configuration options")
    and drops the line that names the keyword. Run `ssh -G -F` in `doctor` and
    report its full stderr.
  - `internal/transport/sshconfig.go:97`: `ssh.legacyKeyTypes`, documented as
    re-enabling ssh-rsa "everywhere", is written only into role blocks, so
    compute nodes never get it. Write it into `Host *` as well.
  - `internal/transport/sshconfig.go:133`: `ssh.options` come after the
    generator's own keywords. `ConnectTimeout`, `StrictHostKeyChecking` and the
    other keywords the generator already writes are silently ignored there,
    while the same key in a role wins. Reject such keys, or merge them into the
    generated values.
  - `internal/transport/sshconfig.go:205`: sub-second durations are truncated
    to zero. `ConnectTimeout 0` means no timeout, and `ControlPersist 0` keeps
    the master indefinitely. An explicit `controlPersist: 0s` falls back to 5
    minutes. Round up to 1 s or reject such values, and allow persistence to be
    switched off.
  - `internal/transport/sshconfig.go:151`: `ssh.include` entries are checked
    against the working directory, not against the Site directory as
    `doc/configuration.md` says. ssh then reads a relative entry from `~/.ssh`,
    and globs are dropped. Resolve entries with `a.Path` and pass globs through
    to ssh.
  - `internal/app/app.go:199`: with a relative `--config` or
    `CLUSTERCTL_CONFIG`, the persistent generated file holds a relative
    `UserKnownHostsFile`. The `ssh -F` line that `troubleshooting.md` says to
    rerun by hand then checks a different file when run from another directory.
    Make the base directory absolute when loading.
- **6.13 Host key file rewrite drops comments and misparses markers**
  (`internal/hostkeys/hostkeys.go:87`). `Parse` keeps `#` lines only before the
  first entry, so every `hostkey refresh`, `hostkey remove` and reinstall
  rewrites the versioned file without the comments between entries. `@revoked`
  and `@cert-authority` lines are split by position, so `hostkey verify` reports
  "matches" for a key the same file revokes, though ssh itself still refuses it.
  Hashed and wildcard entries are reported as missing. Separately, with
  `ssh.strictHostKeyChecking: false`, the generated file sets `accept-new` while
  `UserKnownHostsFile` still points at the shared site file
  (`internal/transport/sshconfig.go:112`). ssh then appends to that file outside
  any clusterctl lock, and an append that lands during another administrator's
  locked rewrite is lost. Keep body comments with the following entry, parse
  markers into their own field and honour `@revoked` in `verify`. With
  `accept-new`, point `UserKnownHostsFile` at a per-user file and keep the site
  file as a read-only `GlobalKnownHostsFile`.
- **6.14 copy and login mishandle some arguments, and copy runs serially**
  (`internal/cli/copy.go:86`).
  - `internal/cli/copy.go:86`: a download from several nodes with more than one
    source writes to `DESTINATION<node>-<first source's base name>`, which is a
    file path. scp fails on every node and transfers nothing. Remote paths are
    not quoted either, so under the legacy scp protocol (the default before
    OpenSSH 9.0, as on RHEL 8) the remote shell splits and expands them.
    Download into `<dir>/<node>/`, and force SFTP mode (`scp -s`) or document
    that remote paths are shell-interpreted.
  - `internal/cli/copy.go:83`: `copy` handles the nodes one at a time with no
    timeout and ignores `fanout.max` and `--fanout`. A transfer that stalls on a
    live connection holds up every later node until Ctrl-C. Those nodes are then
    reported as failed without having been tried. Run copies through the bounded
    executor with a timeout for each transfer.
  - `internal/cli/login.go:131`: `login` uses only the first word as the name.
    Further words before `--` are ignored, and without `--` the command is
    dropped and an interactive shell opens. Reject more than one positional
    argument before `--`.
- **6.15 hostkey scan is slow against unreachable hosts**
  (`internal/hostkeys/hostkeys.go:256`). `Scanner.Scan` dials a new connection
  for each of the three algorithms and does not stop at a dial error, and
  `scanTargets` scans hosts one at a time. A filtered or powered-off host
  therefore costs three times `--timeout`; a closed port fails at once. `hostkey
  refresh` over 20 nodes still in the installer can wait about 10 minutes after
  the confirmation. Stop at the first dial error, and scan hosts in parallel
  with a bound.
- **6.16 A tunnel exclude that expands to nothing is dropped**
  (`internal/tunnel/tunnel.go:98`). `workstation.host` is always defined, and it
  is the empty string when no Workstation document matches this machine.
  `{workstation.host}` then expands to nothing, the exclude is silently skipped,
  and the workstation's own address is routed into the tunnel. `--dry-run` shows
  this, and sshuttle's nat method already returns local traffic, so the impact
  is small. Treat an empty expansion as an error, or warn.
- **6.17 transport.md promises a clush offload that does not exist**
  (`doc/transport.md:116`). The doc says a set larger than `fanout.offloadAbove`
  can be run through `clush` on a host role. Nothing reads `offloadAbove` or
  `offload`, although `defaults.yaml` sets 512 and the example site sets
  `offload: login`, so a large fan-out still opens one ssh connection per node
  from the workstation. `doc/requirements.md` marks R21 deferred. Implement the
  offload, or remove it from the doc, the defaults and the example.

## 7. Service processors

This section covers the `bmc` commands over Redfish and IPMI, and the
credentials they use. Defects here either act on the wrong machines, or report a
power action as done when it was not.

### 7.1 IPMI actions report success for failures

**Medium** · `internal/ipmi/ipmi.go:223`, `internal/ipmi/ipmi.go:259`,
`internal/cli/bmc.go:229`

`runScript` treats a backend run as failed only when it failed and printed
nothing. A backend that `timeout` kills (exit 124, at the default
`bmc.ipmi.timeout` of 60 s) after partial output therefore passes as a normal
result. `ipmiPower` counts failures only among the lines it parsed and never
checks that every requested BMC answered. BMCs that were never contacted drop
out of the table, and the command exits 0. Empty output with exit 0 also counts
as success. The ipmitool backend loops over the hosts in sequence with `-R 1`,
so a few dozen unreachable BMCs easily exceed 60 s. `isFailure` also recognises
only a list of keywords. ipmitool's "Set Chassis Power Control to Down/Off
failed: ..." and the shell's "No such file or directory" for a wrong
`ipmitoolPath` match none of them; the missing-binary message is piped through
`tail` with `2>&1`, so the script exits 0. Both become rows with the message in
STATE and exit 0, so a script that branches on the exit code, as `safety.md`
intends, takes the nodes to be off.

**Shown by:** In a harness test the backend answered one line and exit 124. `bmc
power off --ipmi -n exe[0001-0004] -y` then exited 0 with a single row, and
empty output gave exit 0 with no rows. A backend answering "BMC busy", "cipher
suite id unavailable", the ipmitool "failed" line and the missing-binary line
gave exit 0, with all four texts in STATE and an empty ERROR column.

**Fix:** Compare the parsed BMCs with the requested set and report every missing
one as failed. Treat a non-zero backend exit, 124 and 137 in particular, as a
failure of every BMC not reported. Recognise the known success outputs for each
backend and action instead of a list of failure words, and scale the timeout of
the ipmitool loop with the size of the set.

### 7.2 Credentials are resolved concurrently

**Medium** · `internal/app/bmc.go:23`, `internal/credentials/credentials.go:75`

`forEachBMC` runs up to `bmc.redfish.maxConcurrent` goroutines (default 8), and
each one resolves the BMC credential. `App.Credentials()` creates the resolver
lazily without a lock, which is a data race. `Resolver.Get` releases its lock
after a cache miss and reads the password outside it, so every concurrent caller
reads. A `prompt: true` credential then prompts up to 8 times at once, and a
`command` helper runs up to 8 times, although `Get` promises to read the
password "once per process". Concurrent `term.ReadPassword` calls save and
restore each other's terminal modes, so the typed password can be echoed, or the
terminal can be left without echo. This needs a BMC credential with `prompt` or
`command`; the example site uses `fromEnv`.

**Shown by:** `go test -race` on `bmc status -n exe[0001-0008]` reported a data
race between `internal/app/bmc.go:23` and `:24` in `forEachBMC`. Eight
concurrent `Resolver.Get` calls on a prompt source printed "prompted 8 times for
8 concurrent lookups".

**Fix:** Create the resolver once, in `New` or under `sync.Once`, and hold a
lock or singleflight for each name across the read in `Get`. Better still,
resolve the credentials the set needs once, before fanning out.

### 7.3 The credential helper command is resolved against the working directory

**Medium** · `internal/credentials/credentials.go:166`

The `file` and `ageFile` sources resolve their paths against the Site directory,
as the configuration reference promises. The `command` source passes
`Command[0]` to `exec.CommandContext` unchanged. A relative helper such as
`[scripts/bmc-password]` contains a slash, so Go runs it relative to the
process's working directory. Usually the effect is a helper that is found only
when the administrator runs clusterctl from the configuration directory. Suppose
a site writes the helper as a relative path (no doc shows one), and the
administrator runs clusterctl from a directory where someone planted that path.
The planted program then runs as the administrator and chooses the password sent
to the BMCs.

**Shown by:** A scratch test put the same relative names in both the
configuration directory and the working directory. The `command` credential
resolved to "from-cwd", and the `file` credential to "from-config-dir".

**Fix:** Resolve a `Command[0]` that contains a path separator with `r.path()`,
as `file` and `ageFile` do. Alternatively, require an absolute path or a bare
name looked up in `PATH`, and reject anything else when the configuration is
loaded.

### 7.4 The FreeIPMI configuration file is written without escaping

**Medium** · `internal/ipmi/ipmi.go:151`

`runIpmipower` hands the account to `ipmipower`, the default backend, through a
`--config-file` built with `fmt.Sprintf("username %s\npassword %s\n", ...)`.
FreeIPMI's parser treats an unescaped `#` as the start of a comment, splits on
whitespace, and gives `"` and `\` special meaning. A password containing `#` is
silently truncated, so `ipmipower` authenticates every BMC in the set with the
wrong password. That can trip lockout policies, while the same credential still
works over Redfish. A password with a space, `"` or `\`, or one that starts with
`#`, makes every IPMI call fail with a configuration file error.

**Shown by:** The test used FreeIPMI 1.6.13's `ipmipower --config-file`. A
30-character password failed with "password value too long", but the same
characters behind `ab#` were accepted, so the value had been cut to `ab`. The
passwords `two words`, `back\slash` and `q"uote` gave "too many arguments",
"continuation character '\' used improperly" and "quotation marks used
improperly".

**Fix:** Write each value as a quoted string, with `\`, `"` and `#` escaped by a
backslash; a `#` inside quotes still starts a comment. Reject newlines.
Alternatively, pass the password to `ipmipower` in a form that needs no escaping
and stays out of argv.

### 7.5 `bmc power cycle` is not batched

**Medium** · `internal/cli/bmc.go:141`

Only `bmc power on` goes through `staggeredPowerOn`. `bmc power cycle` makes one
`ipmipower --cycle` call for the whole hostlist, or drives up to 8 BMCs in
parallel over Redfish with no pause. Every node in the set therefore powers on
again within seconds. The command's own help says "Powering many nodes on at
once trips rack breakers", and a cycle of a rack draws the same inrush current.
`safety.md` promises batching only for a power-on, so this is a gap in the
contract rather than a literal breach. Power-cycling a rack of hung nodes is a
common action.

**Shown by:** Under the recording transport, `bmc power cycle --ipmi --stagger
1ms -n exe[0001-0010] -y` recorded one call with `--hostname
'exe[0001-0010].mgmt.hpc.example.org' --cycle`. The same command with `on`
recorded two batches.

**Fix:** Send `cycle` through the same batching and stagger as `on`.

### 7.6 Batched power-on stops silently at the first failing batch and prints one result per batch

**Medium** · `internal/cli/bmc.go:209`

`staggeredPowerOn` returns as soon as any BMC in a batch fails. The later
batches are never sent, and neither the table, the JSON nor the error mentions
them. The error ("1 of 3 service processors failed") counts only the failing
batch, so the administrator takes the rest of the confirmed set to be on. An
unreachable IPMI gateway drops the later batches in the same way, with exit 3.
Each batch also prints its own result, so `-o json` writes several top-level
arrays that do not parse as one value, and `-o jq` and `-o jsonpath` run once
per batch. That breaks the promise in `output.md` that "-o json means the same
thing everywhere". Separately (`bmc.go:175-180`), `--batch 0` and `--stagger 0`
count as "not given", so an explicit zero cannot override the configuration,
although `power.md` says the flags "override for one command". That error is on
the safe side.

**Shown by:** In a harness test one BMC in the first batch timed out. `bmc power
on --ipmi --batch 3 --stagger 1ms -y -n exe[1-10]` exited 1 with "1 of 3 service
processors failed" and listed only `exe[0001-0003]`; batches 2 to 4 were never
sent or mentioned. Without failures, `-o json` printed four top-level arrays,
and `-o jq=map(select(.error))|length` printed `0` four times. `--stagger 0`
still waited 5 s.

**Fix:** Collect the rows of every batch and print them once. Carry on with the
remaining batches or stop on purpose, but report every node either way. Give
untried nodes the state `not tried`, and phrase the error as "N of M failed, K
not tried: <set>". Use `cmd.Flags().Changed` to tell an explicit zero from an
absent flag.

### 7.7 A set with mixed vendors uses the first node's transport and credential

**Medium** · `internal/cli/bmc.go:218`

`bmc status`, `bmc power` and the preview choose the transport from
`a.PreferredBMCTransport(firstNode(nodes))`. `ipmiPower` builds one backend from
`a.IPMIBackend(firstNode(nodes))`, whose credential comes from the first node's
vendor profile. Every BMC in the set then receives that one username and
password on stdin. In a set that spans vendors, another vendor's BMC is driven
over a transport its profile does not allow and is offered the wrong vendor's
password. The preview also names a single transport for the whole set. Over
Redfish, credentials are resolved for each node, so only the choice of transport
is wrong there.

**Shown by:** The test added `vendors.vendor1 {credential: bmc-v1, order:
[ipmi]}` to the example site. `bmc power off -n dbm01,exe0005 --force -y`
(`dbm01` is vendor1, `exe0005` vendor2) recorded one `ipmipower` call for both
BMCs, with `username admin1` and vendor1's password on stdin.

**Fix:** Group the set by transport and credential using each node's profile,
show the groups in the preview, and run one backend per group.

### 7.8 The documented fallback to IPMI is not implemented

**Medium** · `internal/app/bmc.go:166`

R23 ("Prefer Redfish, fall back to IPMI") is marked met, and `power.md` says
"Redfish is tried first". `PreferredBMCTransport` returns only the first entry
of the order, and each caller applies it to the whole set, taken from the first
node. A Redfish failure is reported as a failed target and never retried over
IPMI, not even for read-only `bmc status`, so nodes whose BMC has no Redfish
service fail instead of falling back. Order entries are not validated, so any
first entry other than `ipmi`, a misspelt `impi` for example, selects Redfish.
`bmc power status` and `provision reinstall` ignore the order altogether. R28
justifies not retrying a power action. It does not cover status, the missing
validation, or R23 being marked met.

**Shown by:** `--dry-run --set 'bmc.order=[impi]' bmc power off -n exe0001`
printed "through Redfish", and `config validate` with the same setting reported
the documents valid.

**Fix:** Validate order entries against `redfish` and `ipmi`. Implement the
fallback for `status`, and for actions only when the request provably did not
reach the BMC. Otherwise mark R23 partial and document that only the first entry
is used.

### 7.9 `bmc ping` panics on a blank line and reports a missing `fping` as every BMC down

**Medium** · `internal/cli/bmc.go:763`

`result.Lines()` drops only empty lines. A line of whitespace from the gateway
therefore reaches `strings.Fields(line)[0]` and panics. A plausible trigger is a
shell startup file on the gateway that prints whitespace for non-interactive
sessions. `bmc ping` is a read command offered through the MCP `read_command`,
which runs the command in the server process, and nothing recovers the panic, so
the same output takes down the MCP server. The command never checks
`result.Failed()`. A missing `fping` (exit 127) or an unreachable gateway (ssh
exit 255) is reported as every BMC not answering, with exit 1 instead of 3. Node
names that start with `-` also reach fping's argv without a preceding `--`.

**Shown by:** In a harness test, gateway output of
`exe0001.mgmt.hpc.example.org\n   \n` panicked with "index out of range [0] with
length 0". Exit 127 with "bash: fping: command not found" printed "0 of 2
answered" with exit 1, and exit 255 gave the same.

**Fix:** Skip lines that have no fields. Treat a connection failure, or an exit
code above 1, as a failure of the tool (exit 3). Put `--` before the host list,
or refuse names that start with `-`.

### Lower severity

- **7.10 The Redfish client follows redirects**
  (`internal/redfish/client.go:95`). The `http.Client` uses Go's default
  redirect policy. On a 307 or 308 it sends a POST, a reset included, again to
  the new location. It keeps the `Authorization` header when only the scheme or
  port differs, so a redirect to `http://same-host:port` sends the Basic
  credentials in cleartext, and the answer is accepted without TLS or a pin
  check. Only the already pinned endpoint can issue such a redirect (misbehaving
  firmware, or whatever answered first contact). Set `CheckRedirect` to return
  `http.ErrUseLastResponse`, and never follow a redirect for POST or PATCH.
- **7.11 First-use pinning is check-then-set** (`internal/redfish/pins.go:91`).
  `pinVerifier` reads the pin outside any lock and calls `Set` when none is
  recorded, and `Set` overwrites unconditionally. Two overlapping first contacts
  that present different certificates are therefore both accepted, and the last
  one is recorded. Overlap is reachable, because MCP calls run concurrently and
  `forEachBMC` fans out. It needs a man in the middle at first contact and never
  bypasses a recorded pin, but it weakens later detection. Make `Set`
  compare-and-set inside the `fileutil.Update` callback, and return
  `PinMismatchError` when another pin was recorded in the meantime.
- **7.12 Certificate pinning has no test**
  (`internal/redfish/redfish_test.go:104`). Every Redfish test injects the
  `httptest` server's trusting transport, and the pinning TLS configuration is
  built only when `Transport` is nil. No test covers recording a pin, refusing a
  changed certificate before any request, or withholding credentials on a
  mismatch; pinning works today. Add tests with `Transport` nil against an
  `httptest` TLS server and a temporary `PinStore`.
- **7.13 Reset types from the vendor profile are never used**
  (`internal/redfish/system.go:121`). Nothing reads `VendorProfile.ResetTypes`,
  although ADR 0005, `power.md` and the example's `vendor2` rely on it. `Reset`
  checks only an inline `ResetType@Redfish.AllowableValues` and does not follow
  `@Redfish.ActionInfo`. On firmware that lists its types only through
  `ActionInfo`, `bmc power soft` sends `GracefulShutdown` unchecked, and the
  BMC's own opaque rejection comes back. Pass the profile's list to the client
  and check against it, and follow `@Redfish.ActionInfo` when the inline list is
  absent.
- **7.14 Configuration promises the code does not keep**
  (`internal/apis/v1alpha1/types.go:169`). There are three:
  - The `bmc.ipmi.via` comment says that an empty value "runs it locally", but
    `IPMIBackend` then returns a usage error.
  - `bmc.ipmi.passwordTransport` accepts `file`, `stdin` and `env`, but nothing
    reads it, so the password always goes through a mode-0600 file on the
    gateway.
  - `power.md` shows batch 8 over ten nodes as 8 then 2, but the code splits
    them evenly into 5 and 5.

  Fix the comments or implement local execution, implement or remove
  `passwordTransport`, and correct the example.
- **7.15 Interrupted resets are reported like unsent ones**
  (`internal/cli/bmc.go:324`). `forEachBMC` starts every node without checking
  the context and stores the text of every error. After Ctrl-C, a BMC whose
  reset POST was already in flight and a BMC never contacted both read
  "interrupt signal received" and count as failed; the in-flight case was traced
  in the code, not run. Re-running for the "failed" nodes then power-cycles the
  in-flight ones a second time. `provision reinstall` behaves the same way, and
  credentials are still resolved for the rest of the set after the interrupt.
  Check the context before starting each node and mark unstarted nodes "not
  sent". Report actions that were in flight as "outcome unknown, check power
  state".

## 8. Secrets

This section covers the sops-encrypted `Secret` documents: how clusterctl
decrypts them, how much of the plaintext reaches its output, and how `secrets
push` writes them to nodes. A Secret holds BMC passwords, munge keys and
keytabs. ADR 0013 promises that a file edited without sops is refused and that
`secrets check --decrypt` prints nothing of the plaintext.

### 8.1 An unauthenticated sops type tag puts decrypted values into error messages

**High** · `internal/secrets/sops.go:128`, `internal/config/secretdoc.go:75`

sops stores each value as `ENC[AES256_GCM,data:...,iv:...,tag:...,type:T]`, and
the GCM tag does not cover the type field. If `type:str` is changed to `int`,
`float`, `bool` or `time`, sops decrypts the value and then parses the plaintext
with `strconv` or `time`. The parser's error quotes the value. `tree.Decrypt`
returns that error at line 128, before the MAC is compared at line 137, and line
130 passes it on verbatim. The load-time check at `secretdoc.go:75` tests only
the `ENC[` prefix and the `]` suffix, so `config validate` still passes. The
plaintext then appears in the `STATUS` column and the JSON `status` field of
`secrets check --decrypt`. It also appears in the per-node error of any `bmc`
command that resolves a `secretRef` credential, and MCP `read_command` returns
both of these to the agent. The attacker needs write access to the configuration
repository but no key. Someone who holds a key must then run one of these
commands where the attacker can read the output, for example in CI logs or an
MCP client.

**Shown by:** A scratch test changed `type:str]` to `type:int]` on a
`bmc-password: hunter2` value. `config validate` printed `9 documents are
valid`, and `secrets check --decrypt -o json` reported `Could not decrypt value:
strconv.Atoi: parsing "hunter2": invalid syntax`. The `bool`, `float` and `time`
variants and `bmc status -n exe0001 -o json` leaked the value in the same way,
and `type:bytes` made `DecryptSops` panic with `hash of unhashable type
[]uint8`.

**Fix:** Replace the sops error with a fixed message such as "the file could not
be decrypted or was changed without sops", so that nothing from a decrypted
value reaches output. At load, refuse any value under `data` or `binaryData`
whose type tag is not `str`. The tag is readable without a key.

### 8.2 Decrypted YAML is parsed a second time, and the parse error carries the plaintext

**Medium** · `internal/config/secretdoc.go:129`, `internal/secrets/sops.go:140`

`DecryptSops` writes the plaintext out as YAML with the sops store, and
`SecretValues` parses it again with `goccy/go-yaml`. For some values the two
libraries disagree. goccy's error then quotes the surrounding lines of the
decrypted document, which include the plaintext of other keys when they sit
close enough. `app.SecretValues` passes that error to the `STATUS` column of
`secrets check --decrypt`, and so to MCP `read_command`. The trigger is narrower
than first reported. Of 819 short test strings, only values made entirely of
newlines (`"\n"`, `"\n\n"`, `"\n\n\n"`) made goccy fail. Values with trailing or
interior blank lines came back unchanged. Two other kinds of value come back
changed or are refused, without leaking anything: a U+2028 separator comes back
with spaces inserted after it, and a value that starts with a tab and contains a
newline is emitted as `null` and rejected with "a value must be a string".

**Shown by:** A scratch test with `data: {bmc-password: hunter2, spacer: "\n"}`
and a `binaryData` munge key ran `secrets check --decrypt -o json`. The status
read `fails: .../secrets.sops.yaml: [9:1] could not find multi-line content`,
followed by the source lines `bmc-password: hunter2` and `munge-key:
czNjcjN0LWtleQ==`.

**Fix:** Read the values directly from the decrypted `sops.Tree`
(`tree.Branches`) instead of writing YAML and parsing it again. Never put source
text of a decrypted file into an error that is printed or returned to MCP.

### 8.3 The Vault token is sent to whatever address the file's metadata names

**Medium** · `internal/secrets/sops.go:119`, `internal/secrets/sops.go:87`

`DecryptSops` gives sops `keyservice.NewLocalClient()`, which tries every master
key named in the file's metadata. The sops MAC does not cover that metadata, and
`InspectSops` accepts any key type. In sops v3.13.3, `SOPS_HC_VAULT_ALLOWLIST`
defaults to `all`, and because of the nil decryption order (8.4) an `hc_vault`
key is tried before the age key. An `hc_vault` entry added under `sops:`
therefore makes clusterctl send the administrator's `VAULT_TOKEN` or
`~/.vault-token` to the address in that entry. Decryption still succeeds through
age, so nothing looks wrong. This happens in every command that reads the
Secret, including `bmc` commands that resolve a `secretRef` credential, `secrets
push` and `provision reinstall`. The sops CLI tries age and PGP first, so it
does not contact the address when an age key works. The precondition is write
access to the shared configuration. That access already lets an attacker run a
program on the workstation through `credentials.*.password.command`, so what
this adds is silent theft of the Vault token.

**Shown by:** A scratch test encrypted a Secret to one age recipient, added an
`hc_vault` entry that pointed at a local HTTP server, and set
`VAULT_TOKEN=hvs.ADMIN-SECRET-TOKEN`. `InspectSops` listed both keys without
error, `DecryptSops` succeeded with the age identity, and the server received
the token three times in `PUT /v1/transit/decrypt/k` requests.

**Fix:** Offer sops only the key types the site uses. Either drop other master
keys from `tree.Metadata` before `GetDataKeyWithKeyServices` or use a key
service that refuses them, and have `InspectSops` reject key types the site does
not allow. At the least, set `SOPS_HC_VAULT_ALLOWLIST=none`, or a configured
allow-list, at start-up.

### 8.4 Age identities are not tried first

**Medium** · `internal/secrets/sops.go:121`

`GetDataKeyWithKeyServices` is called with a nil decryption order. sops
therefore tries the master keys of a group in the fixed order in which it builds
the group: `kms`, `gcp_kms`, `hckms`, `azure_kv`, `hc_vault`, `pgp`, then `age`.
Putting the `workstation.identities` key service first does not change this. For
each master key that is not age, that service declines, and the local sops
client contacts the backend. Any Secret encrypted both to age and to another key
type therefore gets a network call with the workstation's ambient cloud or Vault
credentials, or a `gpg` pinentry prompt, before the age identity is tried,
whatever order the file lists the keys in. ADR 0013 says the identities of
`workstation.identities` are tried first and that sops looks for keys itself
only afterwards. That does not hold, and an air-gapped workstation waits for
each backend to fail first.

**Shown by:** A scratch test added an `hc_vault` key to an age-encrypted file
and called `DecryptSops` with the matching age identity and `VAULT_TOKEN` set.
The call succeeded in 2.9 ms, after one `PUT /v1/transit/decrypt/recovery` to
the Vault server that carried the token.

**Fix:** Pass a decryption order to `GetDataKeyWithKeyServices`: `["age",
"pgp"]`, as the sops CLI does, or `["age"]`. Local identities are then tried
before any network or agent backend.

### 8.5 `secrets push` reports an unreachable node as write failures

**Medium** · `internal/cli/provision.go:165`, `internal/cli/helpers.go:117`

`secrets push` runs one fan-out per secret file (line 144) and counts each
failed node and file pair. It always returns exit code 1 with `%d writes
failed`, which names no host. It does so even when every failure is an ssh
connection failure that the transport has classified as exit code 3.
`doc/safety.md` promises 3 when "a host could not be reached", and says this
distinction lets a wrapper tell a node that said no from a node that was not
there. An unreachable node is also contacted again for every secret, and each
attempt waits out the ssh connect timeout again. The table and JSON rows are
complete and name each failing node, which limits the harm. `exec` also exits 1
for an unreachable node, because `failureError` always returns exit code 1.

**Shown by:** A scratch test with two `secretRef` files answered `exe0002` with
exit 255 and a transport error. `secrets push -n exe[1-3] -y` exited 1 with `2
writes failed`, showed a `Connection timed out` row for each file and made 2
calls to `exe0002`. With the same reply, `exec -n exe[1-3] -- true` exited 1
with `1 of 3 hosts failed: exe0002`.

**Fix:** Count failures per host and name the failed nodes in the error. Return
exit code 3 when the failures are transport failures, both here and in
`failureError`. Skip the remaining secrets for a node that could not be reached.

### Lower severity

- **8.6 Values that look like numbers, booleans or dates are retyped**
  (`internal/config/secretdoc.go:146`). When `sops --encrypt` reads the
  plaintext file, it stores an unquoted value such as `0600`, `007`, `True` or
  `2001-12-14` with an `int`, `bool` or `time` type tag. `FormatValue` then
  renders the decoded Go value, so the node receives `384`, `7`, `true` or
  `2001-12-14T00:00:00Z`, contrary to "used as written" in ADR 0013 and
  `doc/configuration.md`. sops converts the value at encryption time and `sops
  -d` shows the converted value, so the change can be traced, and the usual
  result is a credential that fails to authenticate. Refuse non-`str` type tags
  at load in `checkSecret`, the same check 8.1 needs, and document that such
  values must be quoted.
- **8.7 Under `mac_only_encrypted` the MAC does not cover the kind and name**
  (`internal/secrets/sops.go:95`). In that sops mode the MAC covers only the
  encrypted values. `apiVersion`, `kind`, `metadata.name` and other clear fields
  can then be edited without a key, and `DecryptSops` accepts the file, so ADR
  0013's "a value or a name edited without sops is refused" does not hold. This
  needs the site to opt into the mode, the encrypted values stay authenticated,
  and a repository writer can already redirect a `secretRef` by editing the
  unauthenticated Site document. Refuse a Secret whose sops metadata sets
  `mac_only_encrypted`.
- **8.8 `secrets push` empties the target before writing, and `install -D`
  creates directories readable by everyone** (`internal/cli/provision.go:134`,
  `internal/cli/provision.go:311`). The script runs `install -D -m MODE
  /dev/null TARGET` and then `cat > TARGET`. If the local ssh is killed, or the
  connection drops after `install` and before the payload arrives, `cat` sees
  end of file and exits 0, which leaves an empty key. Its row then reads
  `failed: ` with no reason, because only stderr is shown. The window is narrow,
  and `cinc config` writes its file the same way. `install -D` also creates
  missing parent directories with mode 0755 regardless of `umask 077`. munged
  still accepts such a directory, so munge keeps working, but the directories
  are more open than the script intends. Write to a temporary file in the same
  directory, check its size, then `chmod`, `chown` and `mv` it into place.
  Create the parent explicitly with `install -d -m 0700`, and include `res.Err`
  in the status.

## 9. Configuration

This section covers how configuration is found, layered, validated and
explained, and where clusterctl keeps its state and cache. The configuration
carries the safety settings, `safety.protectedHosts` among them, and names
programs that clusterctl runs locally. A layer that is lost or replaced without
a message therefore changes what a command does.

### 9.1 A mapping override replaces the whole subtree and removes protections

**High** · `internal/config/merge.go:147`

Context overrides, the `overrides` of `Cluster` and `Workstation` documents, and
`--set` all go through `Tree.SetPath`. It stores the value at its path as one
unit and drops the origins recorded beneath that path. The override tables are
typed `map[string]any`, so a nested mapping such as `safety: {confirmAbove: 4}`
passes validation. It then replaces the whole merged `safety` section, although
`doc/configuration.md` says "Mappings merge key by key". An administrator who
writes an override in the nested form that every other document uses gets no
error. `protectedHosts`, `slurmAware`, `powerOnBatch` and `powerOnStagger` are
all lost: a protected host is no longer refused, and with `powerOnBatch` at 0,
`staggeredPowerOn` powers every node on at once. `config validate` passes, and
`config explain safety.confirmAbove` reports that nothing is set there.

**Shown by:** A Config document overrode context `cluster2` with `safety:
{confirmAbove: 4}`. `bmc power off -n wlm01 --dry-run` changed from `power off
would touch the protected host wlm01; pass --force` (exit 2) to `Would power off
1 host: wlm01` (exit 0). `--set 'safety={confirmAbove: 4}'` had the same effect,
and `config view -o json` showed `safety` as `{"confirmAbove": 4}`.

**Fix:** Merge a mapping value key by key with `mergeValue` and keep the
origins, or refuse override values that are neither scalars nor lists. Validate
each override path and value against the schema before applying it.

### 9.2 Override keys that differ only in case bypass the schema

**High** · `internal/config/merge.go:144`, `internal/config/effective.go:266`

Override and `--set` paths are checked only by `validatePath`, which rejects
empty elements, and then by `decodeInto`. `decodeInto` uses `encoding/json`,
which matches field names without regard to case. When two keys match one field,
the key that comes later in the sorted JSON wins. A lower-case variant such as
`safety.protectedhosts: []` therefore overrides the site's `protectedHosts`, and
an upper-case variant such as `safety.ProtectedHosts` or `fanout.Max` is
discarded without a message. Provenance is recorded under the literal path, so
`config explain` is wrong in both cases. It shows the site's protected hosts
after they were removed, and it shows `fanout.Max` as set by the context
although it has no effect. A true misspelling such as `protectedHostz` is
rejected. Environment variables map to fixed paths and are not affected.

**Shown by:** A context override `{safety.protectedhosts: [], fanout.Max: 2}`
passed `config validate`. `config explain safety.protectedHosts` answered
`[wlm01,dbm01] layer site`, and `bmc power off -n wlm01 --dry-run` printed
`Would power off 1 host: wlm01` (exit 0). With `--set
'safety.ProtectedHosts=[exe0001]'`, the same command on `exe0001` was not
refused. With `safety.protectedHosts=[exe0001]` it was.

**Fix:** Before `SetPath`, check every override and `--set` path against the
schema with case-sensitive matching, reusing `unknownKeys` and `suggest`, and
report a failure at the position the path was written. Alternatively, decode the
merged tree with a case-sensitive decoder.

### 9.3 Two documents of the same kind and name replace each other without a warning

**High** · `internal/config/load.go:84`

`load` indexes `Site`, `Cluster`, `NodeInventory`, `Workstation` and `Secret`
documents by `metadata.name` with a plain map assignment (lines 84 to 92). A
later document of the same kind and name therefore replaces the earlier one
whole, with no merge, error or warning. Only `Config` contexts are documented to
replace one another by name, and `doc/configuration.md` says that files may be
read in any order. `config validate` lists both documents and reports them
valid. Two realistic triggers each remove a site's safety section:

- a stale copy such as `site_old.yaml`, which sorts after `site.yaml`;
- a personal directory, read after `/etc/clusterctl`, that holds a `Site` of the
  same name. A `config init` scaffold with its default names is one example (see
  9.8).

The only hint is that provenance points at the winning file.

**Shown by:** A copy of `examples/site` gained a `site_old.yaml` with the same
`Site example`, but without `wlm01` in `protectedHosts`. `config validate`
listed both files and reported them all valid. `config explain
safety.protectedHosts` gave `[dbm01]` from `site_old.yaml`, and `bmc power off
-n wlm01 --dry-run` printed `Would power off 1 host: wlm01` (exit 0). A default
`config init` scaffold layered after `examples/site` also replaced the
inventory: `node select @inventory:exe` printed `no node has class=exe`.

**Fix:** Report a second document of the same kind and name as a validation
error that names both positions. Replace by name only for `Config` contexts, as
documented.

### 9.4 With `HOME` unset, state and cache fall back to shared paths in /tmp

**High** · `internal/config/paths.go:62`, `internal/config/paths.go:74`,
`internal/fileutil/fileutil.go:129`

When `HOME` is unset or empty and `XDG_STATE_HOME` and `XDG_CACHE_HOME` are not
set, `StateDir` and `CacheDir` return `$TMPDIR/clusterctl` and
`$TMPDIR/clusterctl-cache`. These are fixed paths shared by every user. This
happens under `env -i` wrappers and in systemd system units without `User=`,
such as a long-running `clusterctl mcp`. `EnsureDir` and `WriteAtomic` call
`os.MkdirAll`, which accepts an existing directory whoever owns it and whatever
its mode, and `doctor` reports it `ok`. A local user who creates the directory
first with mode 0777 can then replace the files clusterctl trusts in it:

- the generated `ssh_config`, whose `ProxyCommand` `ssh -F` runs as the
  administrator;
- the Redfish certificate pins;
- the control sockets and the tunnel pid files;
- the cached `dhcpd.conf`;
- the group cache. An entry dated in the future never expires
  (`internal/groups/groups.go:374`), so `@group` resolves to hosts the attacker
  chose.

`doc/configuration.md` promises that both directories are created with mode 0700
and does not mention the fallback. Relative `XDG_STATE_HOME` and
`XDG_CACHE_HOME` values are also accepted. The preconditions are narrow: `HOME`
must be unset on a multi-user host that has a hostile local user. The result is
code execution as the administrator.

**Shown by:** `$TMPDIR/clusterctl` was created beforehand with mode 0777. `env
-i PATH=/usr/bin:/bin TMPDIR=... clusterctl --config examples/site login
--dry-run install -- true` then printed `ssh -F <tmp>/clusterctl/ssh_config
...`, the directory stayed `drwxrwxrwx`, and `doctor` reported `state directory
ok`. An `ssh_config` owned by `nobody` with mode 0666 was then renamed into that
directory, and `ssh -F` ran its `ProxyCommand` as root.

**Fix:** Refuse to run without a home directory or absolute `XDG_STATE_HOME` and
`XDG_CACHE_HOME`, and ignore relative `XDG_*` values as the XDG specification
requires. In `EnsureDir`, `Lstat` the directory and require a real directory,
not a symlink, that the effective uid owns and that has no group or other write
permission. Have `doctor` report any violation.

### 9.5 A misspelled `--config` entry is skipped without a message

**Medium** · `internal/config/paths.go:135`, `internal/app/app.go:140`

`ExpandEntries` skips any entry that does not exist, because the default search
path names places that a site may not use. Entries named explicitly with
`--config` or `CLUSTERCTL_CONFIG` go through the same function. A misspelled
file therefore disappears without a message, together with its `currentContext`,
its context overrides or its `Workstation` overrides. The command then resolves
to a different context, typically the site's default, and the confirmation
preview does not name the context. An error appears only when every entry is
missing, and it points at the default directories, not at the misspelled path.

**Shown by:** A `test-ctx.yaml` set `currentContext: cluster2`. Naming it
`test-ctx.yml` by mistake, `--config examples/site --config .../test-ctx.yml
config validate` printed `7 documents are valid; context cluster1 resolves`
(exit 0), where the correct name gives `8 documents are valid; context cluster2
resolves`. `CLUSTERCTL_CONFIG` behaved the same, and `bmc power off -n exe0001
--dry-run` gave no warning.

**Fix:** Skip missing entries only on the built-in search path. Report a missing
entry from `--config` or `CLUSTERCTL_CONFIG` as a usage error.

### 9.6 The `config init` Cluster does not pin its inventory

**Medium** · `internal/config/scaffold/cluster.yaml.tmpl:11`

The scaffold names its `NodeInventory` after the site, but its `Cluster` does
not set `inventories`. `loadInventory` (`internal/app/app.go:251`) then takes
every `NodeInventory` that was loaded, from any site. That default is
documented, but reading several sites together is a stated requirement (R59). In
that case each cluster's `@class` groups include the other site's nodes, and
those names are resolved through this site's naming rules and domain. `config
validate` accepts the combination without a warning, and a command on one
cluster selects hosts of another site.

**Shown by:** Two scaffolds were loaded together, one written with `--site sitea
--cluster alpha` and one with `--site siteb --cluster beta`, after `gpu[01-04]`
of class `compute` had been added to site b's inventory. `--context alpha node
fqdn -n @compute` printed `gpu[01-04].a.example.org`, and `bmc power off -n
@compute --dry-run` printed `Would power off 4 hosts: gpu[01-04]`.

**Fix:** Write `inventories: [<site>]` into the scaffold's `Cluster`. Consider
making an empty list mean the inventories of the cluster's own site rather than
every loaded one.

### 9.7 Configuration is read without an ownership or permission check

**Medium** · `internal/config/paths.go:155`, `internal/cli/config.go:154`

The configuration names programs that clusterctl runs locally: `ssh.binary`,
`ssh.scpBinary`, `credentials.*.password.command`, `workstation.sshuttleBinary`,
and `groups.sources.*.exec` when no role is set. `yamlFilesIn` loads every
`.yaml` file in a configured directory. It does not check that the file and the
directory are owned by the user or root and not writable by others, as OpenSSH
does for `~/.ssh/config`. `refuseNonEmpty` checks only that a directory is
empty, so `config init` also writes into an empty directory owned by another
user and then prints `export CLUSTERCTL_CONFIG=<dir>`. An administrator who
points clusterctl at a directory another user can write to, such as a
world-writable checkout or an empty directory in `/tmp`, runs that user's
programs with the administrator's own credentials and agent.

**Shown by:** Running as root, `config init /tmp/zz_adv_f5/cfg` wrote into a
0777 directory that `nobody` had created, and printed the `export` line.
`nobody` then added a `Workstation` with `overrides: {ssh.binary:
/tmp/zz_adv_f5/x}`. When root next ran `doctor`, it ran `x`, which recorded
`uid=0(root)`.

**Fix:** Refuse configuration files, and the directories that hold them, when
they are writable by group or other or owned by anyone other than the user or
root. Make `config init` refuse a directory it does not own.

### 9.8 `config init` without `DIR` overrides a team configuration in `/etc/clusterctl`

**Medium** · `internal/cli/config.go:133`

With no `DIR`, `--config` or `CLUSTERCTL_CONFIG`, `initDir` writes into the user
configuration directory, and `refuseNonEmpty` checks only that directory.
`/etc/clusterctl`, which is read first, is never consulted. ADR 0017 refuses to
write into one of several `--config` or `CLUSTERCTL_CONFIG` places, because a
complete configuration there changes what the others resolve to. The same
situation on the default search path is allowed. The scaffold is a complete
configuration: its `Config` sets `currentContext`, its `Site` has
`protectedHosts: []`, and its default names `example` and `cluster1` are the
ones `examples/site` uses. Suppose a new administrator follows the README on a
host where the team keeps `/etc/clusterctl`. If the names match, the team's
`Site` and inventory are replaced (see 9.3). If they differ, the current context
moves to the scaffold's site. Either way, the team's protected hosts no longer
apply.

**Shown by:** The default search path was simulated as `--config examples/site
--config <scaffold>`. `exec --dry-run -n wlm01 --confirm -- reboot` changed from
a protected-host refusal (exit 2) to `Would run a command on 1 host: wlm01`
(exit 0). `config explain safety.protectedHosts` gave `[]` from the scaffold's
`site.yaml`, and `config validate` gave no warning. With `--site lab --cluster
alpha`, `config contexts` marked `alpha` as current.

**Fix:** Without `DIR`, refuse, or require `DIR`, when another entry of the
search path, `/etc/clusterctl` included, already provides configuration.
Alternatively, write only the scaffold's `Config` or `Workstation` there, and do
not ship an empty `protectedHosts` that overrides a merged `Site`.

### 9.9 Lock files are private, and atomic writes change owner and mode and replace symlinks

**Medium** · `internal/fileutil/fileutil.go:112`,
`internal/fileutil/fileutil.go:53`

`Lock` creates `.<name>.lock` with `gofrs/flock`, which opens it with mode 0600.
The first administrator to write a shared file therefore owns a lock file that
no other user can open. The documents put the shared host key file at
`/etc/clusterctl/ssh-known-hosts`, and R05 promises that two administrators
writing it at once lose no entry. In practice, a second administrator gets
`permission denied` from every later `hostkey refresh`, every `hostkey remove`,
and the host-key step of `provision reinstall`. The failure comes before
anything changes, so no entry is lost. `WriteAtomic` renames a new temporary
file over the target. That also changes the file's owner to the writer, forces
its mode to `perm` (0644 for host keys, which drops group write), and replaces a
symlink with a regular file. The directory is not synced after the rename.

**Shown by:** A scratch test called `fileutil.Update` in a setgid group
directory with mode 2775. As uid 1001 it succeeded and left `-rw------- 1001
2000 .ssh-known-hosts.lock`. As uid 1002, in the same group, it failed with
`locking .../ssh-known-hosts: open .../.ssh-known-hosts.lock: permission
denied`.

**Fix:** Create the lock with `flock.SetPermissions` to match the group
permissions of the target or its directory, or lock the directory itself. When
replacing the file, keep the existing file's mode and group, resolve symlinks
before the rename, and sync the directory afterwards.

### Lower severity

- **9.10 Provenance gaps in `config explain`** (`internal/app/app.go:181`,
  `internal/config/effective.go:201`, `internal/config/effective.go:153`).
  `app.New` applies `--fanout` and the default `fanout.max` of 16 to the decoded
  settings, and neither reaches the configuration tree, so `--fanout 4 config
  explain fanout.max` reports 24 from `site.yaml` while commands use 4. Values
  from a context record no file or line, although `doc/configuration.md` says
  every merged value keeps them; this covers the context's `user` and every
  context override. An unknown override path is reported only as a bare `json:
  unknown field "confirmAbov"`, with no file, line or suggestion. Fix: apply
  `--fanout` with `SetPath` in the flags layer and put the default in
  `defaults.yaml`; keep the Config document's positions for context overrides;
  validate override paths when they are applied and report them with
  `doc.Position`.
- **9.11 Smaller configuration defects** (`internal/config/merge.go:114`,
  `internal/app/app.go:280`, `internal/config/scaffold.go:170`,
  `internal/cli/config.go:354`, `internal/cli/config.go:89`). The merge splits
  map keys that contain a dot, such as a static group `rack.R01`, into nested
  paths, so a schema-valid document fails with a JSON error that gives no
  position. A relative path from `CLUSTERCTL_KNOWN_HOSTS` or `--set` resolves
  against the Site document's directory rather than the working directory, so
  `hostkey refresh` writes into the site checkout. `config init` writes names
  such as `1e3` and `08` unquoted although YAML 1.2 tools such as `yq` read them
  as numbers. `config use-context` prints the context name unquoted in its
  `export CLUSTERCTL_CONTEXT=` line. `config init` exits 1, the code for a
  failed target, rather than 2 when a local `stat`, `mkdir` or write fails. Fix:
  merge by path segments or reject dotted keys at validation; resolve
  environment and `--set` paths against the working directory; always
  double-quote scaffold names; quote the context with `shellQuote`; wrap the
  `config init` errors with `exitcode.Usage`.
- **9.12 Fields that are accepted but never read**
  (`internal/apis/v1alpha1/types.go:298`, `internal/apis/v1alpha1/types.go:489`,
  `internal/apis/v1alpha1/types.go:492`). `safety.requireReason`, `slurm.json`
  and `slurm.partitions` are in the schema and pass validation, but no code
  reads them. Setting `requireReason: false` or `json: true` has no effect and
  gives no warning; `slurm node drain` still demands a reason, which is the safe
  direction. Fix: implement the fields, or remove them from the schema so that
  validation rejects them.

## 10. The MCP server

This section covers `clusterctl mcp serve`: the plan and apply cycle, the
`read_command` tool and the promises `doc/mcp.md` makes about them. The server
treats its agent as untrusted, so a defect here lets an agent act beyond what
the administrator confirmed, mislead the confirmation, or take the server down.

### 10.1 A plan is not bound to the cluster it was made for

**Medium** · `internal/mcpserver/plan.go:405`, `internal/mcpserver/plan.go:456`

The server pins only the context name at startup. Every call, including
`apply_plan`, reads the configuration again and resolves that name afresh. The
plan records neither the cluster nor the Slurm host, and `apply_plan` re-checks
only the protected hosts before `execute` sends the change to whatever
`a.Slurm()` now resolves to. The confirmation question and every result still
name the cluster captured at startup. The configuration may be edited while a
plan waits, which is up to 10 minutes by default, so that the pinned context
points at another cluster or Slurm host. The human then confirms one cluster and
the change runs on another. This breaks ADR 0014's "What is confirmed is what
runs" and the promise in `doc/mcp.md` that a running agent cannot be moved to
another cluster.

**Shown by:** A scratch MCP test pinned to `cluster1` planned `resume exe1`,
which listed `login (login.hpc.example.org): ... scontrol update
nodename=exe0001 state=resume`. The test then pointed context `cluster1` at
`cluster2` in `config.yaml`. `apply_plan` asked "Context cluster1, cluster
cluster1. Continue?" and, on yes, sent `scontrol update` to
`wlm01.hpc.example.org`.

**Fix:** Record the resolved cluster name and Slurm target in the plan, and
refuse at apply time when the current resolution differs. Build the question
from the current resolution, not from the values captured at startup.

### 10.2 A malformed jsonpath crashes the server, and jq runs without bounds

**Medium** · `internal/output/query.go:181`, `internal/output/query.go:31`,
`internal/mcpserver/command.go:67`

`-o` is not pinned, so an agent chooses the output format of every
`read_command`. `parseBracket` strips the quotes from a bracketed field that
starts and ends with a quote. A lone `'` passes that check, and slicing
`inner[1:0]` panics. Neither the MCP SDK nor cobra recovers the panic, so the
server process dies and every pending plan is lost. `writeJQ` calls `code.Run`
with no context, and `read_command` buffers the whole output before cutting it
to 64 KiB. A non-terminating program such as `repeat(...)` therefore keeps
running after the client cancels, and memory grows until the out-of-memory
killer ends the server or other work on the workstation. The same jsonpath also
crashes the CLI.

**Shown by:** `clusterctl --config examples/site version -o "jsonpath={[']}"`
printed `panic: runtime error: slice bounds out of range [1:0]` from
`output.parseBracket`, and the same arguments through `read_command` killed the
server. `read_command ["version","-o","jq=repeat(\"xxxx...\")"]` with a 1 s
client timeout returned `context deadline exceeded`, and the server heap then
grew from 351 MiB to 2781 MiB within four seconds.

**Fix:** Require `len(inner) >= 2` before stripping the quotes, and turn a panic
in any tool handler into a failed result. Run jq with `code.RunWithContext`, and
give `read_command` an output writer that stops the command once
`maxCommandOutput` is exceeded.

### 10.3 Remote output is buffered without a bound

**Medium** · `internal/transport/transport.go:319`, `internal/cli/dhcp.go:171`,
`internal/cli/boot.go:390`

`Client.Run` captures ssh's standard output and standard error in unbounded
`bytes.Buffer`s and copies them into strings, and commands copy the result again
into tables and their own output. The 64 KiB limit in `read_command` applies
only after the command returns, so it protects the agent's context but not the
server's memory. `dhcp log` and `boot log` accept any `--lines`, and a
compromised node controls its own output for the whole command timeout, which is
10 minutes by default. Either can grow the CLI or the MCP server until the
out-of-memory killer ends it, which drops fan-out results and pending plans. The
impact is on availability only; nothing is changed on the cluster and no
credential is exposed.

**Shown by:** A scratch test gave `transport.New` a fake ssh that runs `head -c
300000000 /dev/zero` and called `Client.Run`. It captured all 300000000 bytes,
with 1054 MiB of heap in use.

**Fix:** Capture output through a limited writer per request that records
truncation. Bound `--lines` in `dhcp log` and `boot log`.

### 10.4 An agent-written drain reason reaches the human confirmation unfiltered

**Medium** · `internal/mcpserver/plan.go:258`, `internal/mcpserver/plan.go:454`

Under MCP, only the agent writes the drain reason. `plan_change` only trims it
and checks that it is not empty. `question` then inserts it verbatim between the
summary and the server's own `Context …, cluster …` line, including newlines,
carriage returns and escape sequences. A prompt-injected agent can add lines
that read like clusterctl's own, such as a false context line or "This is a dry
run on the staging cluster; nothing is sent.", and can try to repaint the
summary in a terminal client. The first line and the checkbox title are
generated by the server and stay accurate, so this misleads the confirmation
rather than bypassing it. The same string also becomes the Slurm drain reason.

**Shown by:** `plan_change drain exe[1-8]` accepted such a reason, including
`\x1b[1A\x1b[2K\r` and a second `About to drain 1 host: exe0001` line. The
`apply_plan` question contained every injected line verbatim, and a single yes
drained the 8 hosts.

**Fix:** Reject reasons that contain control characters (anything below 0x20
other than space, and DEL) and cap their length. Quote the reason in the
question, for example as `reason: %q`, and put the server's summary last, next
to the answer.

### 10.5 ssh and password helpers can prompt on the terminal under MCP

**Medium** · `internal/transport/sshconfig.go:107`,
`internal/transport/transport.go:320`, `internal/credentials/credentials.go:166`

`doc/mcp.md` promises "No terminal. Anything that would prompt refuses instead",
but only clusterctl's own streams keep that promise. The generated `ssh_config`
does not set `BatchMode yes`, and no subprocess is started in a new session.
ssh, and a `password.command` helper such as gpg's pinentry, can therefore open
`/dev/tty` on the terminal the MCP client runs in. When a host falls back to
password or keyboard-interactive authentication, for example because the client
did not pass `SSH_AUTH_SOCK`, a `read_command` or `query_slurm` call puts a
password prompt into the client's interface. The call hangs, and the prompt
competes with the client for keystrokes, so a password typed there may end up in
the client's input field.

**Shown by:** A scratch test pointed `Client.Run` at a password-only ssh server
on 127.0.0.1. Under `setsid`, ssh failed within 55 ms with `Permission denied
(password)`. Under a pseudo-terminal it printed `alice@127.0.0.1's password: `,
and the server received the password typed in reply.

**Fix:** Set `BatchMode yes` for non-interactive requests, at least when serving
MCP. Start ssh and credential helpers with `Setsid` so that they have no
controlling terminal.

### 10.6 `read_command secrets check --decrypt` runs sops key discovery

**Medium** · `internal/cli/provision.go:215`, `internal/secrets/sops.go:119`

`secrets check` is marked read, and with `--decrypt` it calls `a.SecretValues`
for every Secret document. Without `workstation.identities`, `DecryptSops` falls
back to sops' own key service. That service runs `SOPS_AGE_KEY_CMD`, reads the
administrator's age and ssh keys, and asks for the passphrase of an encrypted
key through gpg-agent's pinentry or on `/dev/tty`. It also uses gpg, cloud KMS
and Vault with the administrator's credentials. An agent can therefore make a
passphrase dialog appear for the administrator with no confirmation, against the
promise in `doc/mcp.md` that anything that would prompt refuses instead. The
plaintext is not returned to the agent, but on failure the error text lists the
administrator's key paths. Any Redfish read whose BMC credential is a sops
`secretRef` reaches the same path.

**Shown by:** A scratch MCP test used a passphrase-protected age identity in
`~/.config/sops/age/keys.txt`, no `workstation.identities`, `SOPS_AGE_KEY_CMD`
set to a probe script, and `GPG_AGENT_INFO` pointing at a fake agent.
`read_command ["secrets","check","--decrypt"]` returned exit code 0 with status
`decrypts`. The probe script ran, and the fake agent received `GET_PASSPHRASE`
for that `keys.txt`.

**Fix:** Refuse `--decrypt` under `read_command`, or whenever there is no
terminal. Without a terminal, have `DecryptSops` use only
`workstation.identities`, and strip key paths from errors returned to the agent.

### 10.7 Some read commands report unreachable or unknown nodes as success

**Medium** · `internal/cli/provision.go:361`, `internal/cli/boot.go:124`,
`internal/cli/bmc.go:621`

`cinc show` reports every failed node as `not configured`, including an
unreachable one, and exits 0, although exit code 3 means a host could not be
reached. `boot status` shows a node with no known address as `unknown` in the
table but leaves it out of the JSON object, and exits 0 with nothing on standard
error. `bmc redfish info` exits non-zero, but it also leaves a node whose
Redfish client could not be built out of its JSON object. `provision status`
exits 0 and reduces a BMC error to power `unknown`. `read_command` returns JSON,
so an agent sees exit code 0 and a partial or wrong answer, and may base a drain
or resume plan on it.

**Shown by:** A recorder failed exe0003 with exit 255 (`No route to host`).
`read_command ["cinc","show","-n","exe[1-4]"]` then returned exit code 0 with
exe0003 listed as `not configured`. `-o json boot status -n exe1,nosuchnode`
exited 0 with only exe0001 in the object and nothing on standard error.

**Fix:** Return a transport or target exit code for failed nodes. Put every node
in the JSON object, with an `error` field where it failed.

### Lower severity

- **10.8 `CLUSTERCTL_NODES` supplies a default node set to `read_command`**
  (`internal/cli/root.go:89`). The server clears the node set only for its own
  tools. `read_command` builds a fresh command tree whose `root.App()` falls
  back to `CLUSTERCTL_NODES`, which contradicts "No default node set" in
  `doc/mcp.md`; with `CLUSTERCTL_NODES=exe[1-10],sub[1-2]` set, `read_command
  ["node","hw"]` fanned out to all 12 nodes. Have the MCP command tree ignore
  the variable, or refuse a node command that names no nodes.
- **10.9 The audit trail loses events and records a cancelled apply as failed**
  (`internal/mcpserver/plan.go:391`). `doc/mcp.md` says every plan, refusal and
  apply is appended to `audit.jsonl`, but every audit write discards its error,
  so an apply goes ahead unrecorded when the file cannot be written. Several
  refusals are never recorded: an unknown action, a missing reason, a bad
  selection, an unknown or expired plan id, wrong nodes or count, and a plan
  already applied. When the client cancels `apply_plan` after `scontrol` was
  sent, the local ssh is killed and the entry reads `failed: ... command exited
  -1` although the drain took effect, and the plan is used up. Record every
  refusal, report or block on a failed audit write, and run the change under
  `context.WithoutCancel` with its own timeout once the plan is taken, or else
  record the outcome as unknown.
- **10.10 The apply-time gate re-check discards the fresh preview**
  (`internal/mcpserver/plan.go:371`). `apply_plan` reads the configuration again
  and calls `Gate.Preview`, but keeps only the error, so the question still uses
  the `CountRequired` from plan time. After `safety.confirmAbove` was lowered
  from 8 to 2, a plan for 5 hosts was applied on a plain yes. Use the fresh
  preview for the question and the answer check, and refuse if its targets or
  count differ from the plan.
- **10.11 `--fanout` is not pinned** (`internal/mcpserver/command.go:29`).
  `pinnedFlags` omits `fanout` and `app.New` applies any positive value, so an
  agent's `--fanout 100000` overrides `fanout.max`, although `--set
  fanout.max=100000` is refused. `node hw` on 400 hosts peaked at 400 concurrent
  ssh processes instead of 24. That loads the workstation and can exceed sshd
  `MaxStartups`, so nodes are wrongly reported as unreachable. Pin `--fanout` or
  clamp it to the configured `fanout.max`.

## 11. Exit codes, errors and interrupts

This section covers how clusterctl reports the outcome of a command: its exit
code, its error text and how it handles an interrupt. The exit codes are a
documented contract that wrappers and the MCP server branch on, so a wrong code
sends them down the wrong path.

### 11.1 Usage errors exit 1, and a misspelt subcommand exits 0

**Medium** · `internal/cli/root.go:202` · `internal/cli/helpers.go:29`

Errors that cobra and pflag detect carry no exit code. These include an unknown
flag, an unknown top-level command, a wrong argument count, a bad flag value and
`-n` with no value. `report()` hands them to `exitcode.From`, which counts any
uncoded error as a target failure, so they exit 1. Nothing sets
`SetFlagErrorFunc` or wraps the `Args` validators.
`site/content/docs/reference/exit-codes.md` lists "An unknown flag" under 2, and
its wrapper reads 1 as "some nodes are unhealthy". A misspelt subcommand below
the root is worse. `group()` gives every group a `RunE` that prints help and
returns nil, and cobra passes the unknown word to it, so `clusterctl slurm node
drian 'ticket 42' -n exe0007 -y` prints help to stdout and exits 0. A script
that chains `&& clusterctl bmc power off ...` then goes on to the next step. The
MCP `read_command` tool also reports argument-count errors as exit 1.

**Shown by:** With the built binary and `--config examples/site`, each of `bmc
power off --bogus -n exe0001`, `bmc web`, `--fanout abc version`, `bmcx`, `bmc
power off -n` and `version extra` exited 1, while `-o bogus version` exited 2.
`slurm node drian x -n exe0001 -y`, `bmc powr off -n exe0001 -y` and `provision
reinstal -n exe0001 -y` each printed the group's help on stdout and exited 0,
also under `-o json`.

**Fix:** Set `SetFlagErrorFunc` on the root to wrap flag errors as
`exitcode.Usage`, and wrap the `Args` validators the same way. Give group
commands `Args: cobra.NoArgs`, or have their `RunE` return a `Usage` error when
arguments are present. The test harness returns `cmd.Execute()`'s error and
skips `report()`, so add a test that checks `Execute`'s exit code.

### 11.2 A failed command on a role is reported as unreachable, and its stderr is dropped

**Medium** · `internal/app/remote.go:83` · `internal/app/remote.go:50`

`classify` sets `Result.Err` to `<target>: command exited N` for every non-zero
exit, and marks only ssh's 255 as a transport error
(`internal/transport/transport.go:385`). `RunOnRole` and `RemoteFile` test
`result.Err != nil` first and wrap it as `exitcode.Transport`. As a result, any
failed command on an infrastructure host exits 3, "a host could not be reached",
even though the host answered. The `TargetFailed` branch that would report the
tool's stderr cannot run with the real client. The tests miss this because the
`Recorder` leaves `Err` nil. This affects the DHCP, boot, provision and fabric
commands. The MCP server maps 3 to `unreachable:`, so an agent is told that a
reachable host was down. The Slurm client reaches the same wrong code by a
different route (11.6).

**Shown by:** A fake `ssh` wrote `ibwarn: mad_rpc_open_port: can't open UMAD
port` to stderr and exited 1. With it, `clusterctl --config examples/site fabric
counters exe0001` printed `clusterctl: fabric (ibgw01.example.org): command
exited 1` and exited 3, and `dhcp hosts -n exe1` did the same for the DHCP role.

**Fix:** In `RunOnRole` and `RemoteFile`, keep `exitcode.Transport` only when
`result.Err` already carries that code, as `classify` sets it for 255. Otherwise
return `TargetFailed` with `firstNonEmptyLine(result.Stderr, result.Stdout)`.
Give the tests a runner that sets `Err` the way `classify` does.

### 11.3 `exec` never exits 3 or 130

**Medium** · `internal/cli/helpers.go:117`

`failureError` always returns `exitcode.TargetFailed`. It ignores two things:
the `exitcode.Transport` code that `classify` attaches to ssh's 255, and the
`context.Canceled` that the fan-out records for targets it never started.
`exec`, and every command that ends in `failureError`, therefore exits 1 in all
three cases: a node refused, a node could not be reached, or a node was never
tried because of Ctrl-C. `exit-codes.md` and `guides/output.md` document this
exact `clusterctl exec` wrapper with 3 for "some nodes are unreachable", and
`guides/running-commands.md:125` says the same. The MCP `read_command` schema
describes 3 as "a host could not be reached". A wrapper that follows the manual
reports an unreachable rack as unhealthy nodes and never takes its "check the
network first" branch.

**Shown by:** A fake `ssh` printed `Connection refused` and exited 255. With it,
`exec -n exe3 -- uptime` printed `clusterctl: 1 of 1 hosts failed: exe0003` and
exited 1, and under `-o json` the result held `exitCode: 255` while the process
still exited 1. A root command run with an already-cancelled context printed
`exe0001: context canceled` for each node and `2 of 2 hosts failed`, and exited
1.

**Fix:** In `failureError`, return `exitcode.Interrupted` when any failure is
`context.Canceled`. Otherwise return `exitcode.Transport` when the failures
carry a transport code, following a documented precedence. Return `TargetFailed`
only when neither applies.

### 11.4 BMC failures are all flattened to exit 1

**Medium** · `internal/cli/bmc.go:317` · `internal/cli/bmc.go:354`

`forEachBMC` keeps only `err.Error()` for each node, and `printBMCResults`
returns `TargetFailed` whenever any row has an error. Two exit codes are lost on
the way. The `Usage` code that `BMCCredential` attaches to a missing credential
(`internal/app/bmc.go:77`) is dropped, and DNS, connect and TLS failures from
the Redfish client never carry `Transport`. `bmc power`, `bmc status`, `bmc boot
set` and `unset` and each batch of a power-on therefore never exit 2 for a
configuration problem or 3 for a BMC that cannot be resolved or reached. `bmc
boot show` (`bmc.go:536`) and `bmc redfish info`, `get` and `post`
(`bmc.go:640`, `bmc.go:676`) flatten errors the same way. The IPMI path fails
before printing any row and exits 2 for the same missing credential, so the two
back ends disagree.

**Shown by:** `env -u BMC_PASSWORD clusterctl --config examples/site bmc power
off -n exe0001 -y` printed the row `credential "bmc" reads BMC_PASSWORD, which
is not set` and `1 of 1 service processors failed`, and exited 1. The same
command with `--ipmi` exited 2. With the password set, `bmc status -n exe0001`
against a BMC name that does not resolve printed `no such host` and exited 1.

**Fix:** Keep the error value in `bmcResult`, not only its string. Resolve the
credential once before the fan-out and return `Usage` if it fails. Derive the
exit code from the kept errors with the same precedence rule as `exec` (11.3).

### 11.5 An unreachable group-source host exits 2

**Medium** · `internal/app/app.go:353`

`App.Select` wraps every error from `nodeset.ParseWith` as `exitcode.Usage`. An
`exec` group source returns the error from its ssh call
(`internal/groups/groups.go:331`), which `classify` has already coded as
`Transport`. The outer `Usage` wrapper is the first code that `exitcode.From`
finds, so it wins. With the login node down, any command given `-n @slurm:main`
exits 2. `exit-codes.md` reserves 2 for the command line or the configuration,
and the MCP server reports it as `rejected:`, which tells the agent the mistake
was its own. Nothing is changed, so the failure is on the safe side.

**Shown by:** A fake `ssh` printed `Connection timed out` and exited 255. With
it, `clusterctl --config examples/site exec -n @slurm:main -- uptime` printed
`clusterctl: group @slurm:main: source "slurm": login (login.hpc.example.org):
ssh: connect to host login.hpc.example.org port 22: Connection timed out` and
exited 2. `bmc power off -n @slurm:main --dry-run` also exited 2.

**Fix:** In `Select`, wrap as `Usage` only when the error carries no exit code
already. Have the group resolver pass the transport code through unchanged.

### 11.6 Slurm commands misreport partial and transport failures, and lose Slurm's message

**Medium** · `internal/slurm/slurm.go:50` · `internal/cli/slurm.go:160` ·
`internal/slurm/slurm.go:485`

`slurm.Client.run` returns `result.Err` whenever it is set, and `classify` sets
it for every non-zero exit. Over real ssh, Slurm's own message therefore never
reaches the administrator, and every refusal reads `login
(login.hpc.example.org): command exited N`. The CLI then recodes that error
inconsistently:

- `slurm node drain` and `resume`, the accounting changes
  (`internal/cli/slurm.go:191`, `514`, `537`, `560`, `642`, `664`) and MCP
  `apply_plan` (`internal/mcpserver/plan.go:410`) wrap it as `TargetFailed`.
  This hides the `Transport` code of an unreachable login node, so they exit 1
  instead of 3.
- The read commands wrap any refusal as `Transport`, so they exit 3 as if the
  login node were down.

`HasPosixUser` treats `getent`'s exit 2 for an unknown name as a failure. So
`slurm user add ghost proj` exits 3 with `command exited 2` instead of the
documented `the cluster does not know a user account "ghost"`.

Separately, `scontrol update` is not atomic. slurmctld sorts the host list and
stops at the first name it does not know, and the nodes it has already changed
stay changed. An invalid transition, such as resuming a node that is not
drained, is recorded, and the remaining nodes still change. Exit 1 is the right
code for that outcome. But clusterctl prints nothing about which nodes changed,
and `apply_plan` records `failed:` in the audit trail. An administrator who
drains `exe[1-10],zz1` therefore believes the drain failed while ten nodes stay
drained.

**Shown by:** Against a fake `ssh` that refused the connection, `slurm node
drain 'x' -n exe0001 -y` exited 1, while `slurm node list` exited 3. With a fake
`getent` that exited 2, `slurm user add ghost proj` printed `clusterctl: login
(login.hpc.example.org): command exited 2` and exited 3. A recorder answering
`scontrol` with exit 1 made `slurm node drain 'ticket 42' -y -n exe[1-10],zz1`
print only `login (login.hpc.example.org): command exited 1`, with empty stdout.
The Slurm side comes from reading `update_node` in `src/slurmctld/node_mgr.c`,
not from a live test.

**Fix:** In `Client.run`, build the error from the program name, the exit code
and the trimmed stderr unless ssh exited 255, keep any existing exit code rather
than wrapping over it, and treat `getent` exit 2 as "not found". Before the
gate, check the set against `sinfo -h -N -o %N -n <set>` and refuse names Slurm
does not know, both in the CLI and in `plan_change`. After a failed update, read
the states back and report which nodes changed, in the output and in the audit
entry.

### 11.7 A failed node's error is replaced by `command exited N`

**Medium** · `internal/cli/exec.go:199` · `internal/cli/helpers.go:95`

For each failed node, `printExec` takes the last stderr line and then overwrites
it with `res.Err.Error()` whenever `Err` is set. `classify` sets `Err` for every
non-zero exit, so the node's own message is never shown. The administrator sees
`exe0003: exe0003 (exe0003.hpc.example.org): command exited 3` instead of `Unit
slurmd.service could not be found.` `resultsTable` has the same precedence in
its `ERROR` column, and `copy`, `hca config` and `provision` print that table.
Stderr from nodes that exited 0 is never printed in the table formats, so
warnings from successful nodes are lost. The failure lines go to `os.Stderr`
directly (`exec.go:202`) rather than to `a.Err`, the stream that the tests and
the MCP server replace. Only `-o json` and `-o yaml` carry stderr.

**Shown by:** In a recorder test, `exe0003` returned exit 3 with stderr `Unit
slurmd.service could not be found.` and the `Err` that `classify` produces.
`exec -n exe[1-4] -- systemctl is-active slurmd` printed only `exe0003: exe0003
(exe0003.hpc.example.org): command exited 3`, and the output was the same with
`-o wide`.

**Fix:** Show the exit status together with `lastNonEmpty(stderr)` in both
places, and write the failure lines to `a.Err`. Consider showing the stderr of
successful nodes with a marker.

### 11.8 A second Ctrl-C is swallowed

**Medium** · `cmd/clusterctl/main.go:22` · `internal/safety/safety.go:214` ·
`internal/transport/transport.go:320`

`main` calls `signal.NotifyContext` and defers `stop()`. It leaves through
`os.Exit`, though, so `stop` never runs. The first SIGINT or SIGTERM cancels the
context, and every later one goes into the notifier's full channel and is
dropped. This contradicts the comment in `main.go` and
`doc/architecture.md:113`, which promise that "A second interrupt is left to the
default handler". Reads that ignore the context therefore cannot be interrupted
at all:

- the confirmation prompt;
- the BMC password prompt (`internal/app/bmc.go:42`);
- `exec --stdin` (`internal/cli/exec.go:135`);
- `cmd.Wait` in the transport, which has no `WaitDelay` and blocks while a
  descendant of ssh, such as a `ProxyCommand`, holds the output pipe.

`Confirm` does not check the context either. Once the prompt is answered, the
command goes on to its next prompt and then reports the aborted work as
failures. No request is sent with the cancelled context, and Enter, Ctrl-D or
`Ctrl-\` still end the wait.

**Shown by:** In a pty harness, three Ctrl-C at the `Continue? [y/N]` prompt of
`slurm node drain` were ignored, and the process was still running 6 s later.
For `bmc power off` with a prompting credential, Ctrl-C was ignored at the
confirmation and again at `Password for admin@pdu:`. After a password was typed,
the command reported the BMC as failed and exited 1.

**Fix:** Call `stop()` as soon as `ctx.Done()` fires, so that the next signal
gets the default action. Make the prompts and the `--stdin` read select on
`ctx.Done()`, and have `Confirm` return `ctx.Err()` when the context is already
cancelled. Set `cmd.WaitDelay` on every `exec.CommandContext`.

### 11.9 An interrupt is reported as a failure or an unreachable host, not 130

**Medium** · `internal/transport/transport.go:385` · `internal/cli/root.go:195`

`report()` maps only errors that match `context.Canceled` to 130, and most
interrupts never produce one:

- When the context kills a running ssh, `os/exec` returns the `ExitError` of the
  killed process, and `classify` turns it into an uncoded `command exited -1`.
  Only a target that had not started yet gets `ctx.Err()`.
- Redfish requests fail with `context.Cause(ctx)`. Under `signal.NotifyContext`
  that is the signal error `interrupt signal received`, not `context.Canceled`,
  and `forEachBMC` flattens it to a string anyway.

The callers then recode the error. An interrupted `slurm node drain` exits 1.
`RunOnRole` makes an interrupted `boot set` or the PXE step of `provision
reinstall` exit 3; with a real terminal, ssh also receives the SIGINT and can
exit 255 first. The BMC commands report every node as failed. A wrapper that
retries on 3 or alerts on 1 treats the administrator's own Ctrl-C as a cluster
fault.

**Shown by:** SIGTERM during `slurm node drain 'ticket 1' -n exe[0001-0003] -y`
printed `clusterctl: login (login.hpc.example.org): command exited -1` and
exited 1. Ctrl-C at the `bmc boot set Pxe -n exe[0001-0003]` prompt, followed by
`y`, printed three rows reading `interrupt signal received` and `3 of 3 service
processors failed`, and exited 1, although nothing could have been sent. Ctrl-C
at the drain prompt before any ssh started gave `clusterctl: interrupted` and
exit 130.

**Fix:** In `Run`, `Copy` and `Interactive`, check `ctx.Err()` after the command
ends and wrap it with `%w`. In `report()`, test `context.Cause` as well, or give
`main` a context whose cause is `context.Canceled`. Keep the errors, not their
strings, in `forEachBMC`, `secrets push` and `failureError`, so that 130 can be
reached.

### Lower severity

- **11.10 Interrupts kill ssh outright, and fan-out keeps starting targets after
  cancellation** (`internal/transport/transport.go:320`,
  `internal/fanout/fanout.go:64`). `Run`, `Interactive` and `Copy` use
  `exec.CommandContext` with the default `Cancel`, which SIGKILLs ssh. With `-T`
  and no pty, the remote command keeps running until `timeout` ends it or it
  dies of SIGPIPE at its next write, so the comment in `main.go` that remote
  commands "are given the chance to stop" is untrue (the user guide already says
  so). In `RunEach`, `select` picks at random once the context is cancelled and
  a slot is free, so about half the remaining targets still reach `Runner.Run`.
  The shipped client refuses them at `Cmd.Start`, so nothing is spawned, but
  `TestRunHonoursCancellation` cannot detect the problem. Fix: correct the
  comment, or send SIGHUP or SIGTERM through `cmd.Cancel` with `WaitDelay`.
  Check `ctx.Err()` after taking a slot, and add a test that no call is made on
  a cancelled context.
- **11.11 ssh's exit 255 and a remote exit 255 are indistinguishable**
  (`internal/transport/transport.go:378`). `classify` treats every 255 as a
  connection failure, so a remote command or script that exits 255 is reported
  as unreachable. For example, `clusterctl login install -- sh -c 'exit 255'`
  prints `ssh reported a connection failure` and exits 3. Fix: remap a remote
  255 in the wrapper, for example to 254, or look for ssh's own diagnostics on
  stderr before classifying.

## 12. Output, exec, Slurm administration, fabric and doctor

This section covers what the commands print, and defects in individual commands:
`exec`, the output formats, Slurm administration, `fabric` and `hca`, `doctor`,
`dns` and shell completion. Most of these mislead the administrator or a script
rather than change the cluster, but several feed wrong data into the next
command.

### 12.1 Untrusted output reaches the terminal raw, and newlines forge rows

**Medium** · `internal/cli/exec.go:190` · `internal/output/table.go:131` ·
`internal/slurm/slurm.go:112`

Nothing in the program filters control characters. `printExec` and `--dedup`
write node output verbatim, and `writeTable` writes cell values unescaped. The
cells include Redfish strings, BMC error bodies of up to 8 MiB
(`internal/redfish/client.go:198`), drain reasons and, under `-o wide`, the job
`WORKDIR` and `COMMAND`, which any cluster user controls. A compromised node or
BMC can use CR and cursor-up to overwrite the line printed for another node. It
can also use OSC 52 to write the clipboard on terminals that allow it. A newline
in a cell starts a row that appears to belong to another node.

`Nodes()` and `Jobs()` split Slurm's output on newlines and `|` without checking
the field count. `Nodes()` keeps the first row for each name, so a forged row in
one node's drain reason hides another node's real row. `NodeSet()` parses each
name as an expression, so the documented pipeline `clusterctl exec -n
"$(clusterctl slurm node nodeset drain)"` runs on forged hosts. Drain reasons
can be set by operators, health checks, root on any node that holds the munge
key, and clusterctl's own MCP drain. The MCP drain accepts newlines and puts the
reason verbatim into the approval question. Forged job rows need only an
ordinary user account.

**Shown by:** In the harness, `exe0002` answered `ok\r\x1b[1Aexe0001:
\x1b]52;c;...\x07evil`, and `exec` printed the bytes unchanged, with and without
`--dedup`. A table cell containing `bad\nexe0002  idle   fine` rendered as a
healthy `exe0002` row. A fake `sinfo` returned a first reason containing
`\nexe[0001-0064]|drained|...\nwlm01|drained|...`. `slurm node nodeset drain`
then printed `exe[0001-0064],wlm01`, and `exec -n` on that set ran `uptime` on
65 hosts, `wlm01` among them.

**Fix:** In the human formats, replace C0 and C1 control characters other than a
line-ending newline with visible escapes, and escape `\n`, `\r` and `\t` inside
table cells. Reject control characters and `|` in drain reasons, both in the CLI
and in `plan_change`. Parse Slurm output with `--json`, or put user-controlled
fields last and check the field count and that each name is one the query asked
for.

### 12.2 `exec --stdin` swallows read errors and sends a truncated payload

**Medium** · `internal/cli/exec.go:144`

`readAll` is a hand-written read loop. On any error other than the string `EOF`
it breaks out and returns what it has read, with a nil error. A failing standard
input is therefore replayed to every node as if it were complete. A slip of tab
completion that redirects a directory is enough: bash opens the directory, and
the first read fails with `EISDIR`. `clusterctl exec -n @compute --stdin
--script 'cat > /etc/motd' < motd/` then empties `/etc/motd` on every node and
exits 0. An I/O error part way through a file deploys a truncated file
everywhere.

**Shown by:** A scratch test with standard input opened on a directory returned
`err=<nil>` and sent an empty payload to `exe0001`, `exe0002` and `exe0003`. A
reader that returned `partial` and then `input/output error` sent `partial` to
all three, again with no error.

**Fix:** Use `io.ReadAll(a.In)` and return its error before anything is sent.

### 12.3 `-o json` uses Go field names; `-o yaml` turns integers into floats

**Medium** · `internal/transport/transport.go:64` ·
`internal/output/output.go:153` · `internal/output/query.go:283`

`transport.Target` has no `json` or `yaml` tags. Every `Result` that `exec`,
`copy`, `hca config` and `provision` print therefore serialises its target as
`{"Name": ..., "Host": ..., "User": ..., "Role": "", "ForwardAgent": false,
...}`. The documented `jq` path `.target.name` (`guides/output.md:32`,
`guides/running-commands.md:131`) yields `null`, so a script built on it selects
nothing. `Result.Err` is tagged `json:"-"`, so the machine formats carry no
error text.

`writeYAML` round-trips values through `json.Unmarshal` into `any`, which turns
every number into a `float64`. `exitCode` prints as `0.0` and a PID of 1234567
as `1.234567e+06`, so `kill $(yq '.[0].pid')` fails. `toGeneric` does the same
for `jq` and `jsonpath`, which lose precision above 2^53. The YAML emitter also
leaves `.inf`, `.nan` and `? x` unquoted, writes ESC raw and loses CR in block
literals. It does quote the YAML 1.1 words such as `yes` and `no` correctly.

**Shown by:** With `exe0002` failing, `exec -n exe[1-2] -o 'jq=.[] |
select(.exitCode != 0) | .target.name' -- true` printed `null`. `exec -o yaml`
printed `exitCode: 0.0`. `writeYAML` on `{pid: 1234567, big: 9007199254740993}`
emitted `pid: 1.234567e+06` and `big: 9.007199254740992e+15`.

**Fix:** Add lower-case `json` and `yaml` tags to `Target`, with `omitempty` on
`role` and the forwarding flags, and add a serialised error string to `Result`.
In `writeYAML` and `toGeneric`, decode with `json.Decoder.UseNumber` or marshal
the typed value directly. Force-quote strings that a YAML resolver would read as
something else.

### 12.4 `slurm job history --since` sends local time without a zone

**Medium** · `internal/slurm/slurm.go:316`

`History` formats `time.Now().Add(-since)` in the workstation's zone as
`2006-01-02T15:04:05`, with no offset. `sacct` on the login node reads that
string in its own local zone. When the two zones differ, the window moves by the
offset and can start in the future. For example, `clusterctl slurm job history
--state failed --since 1h` run at 12:00 on a CEST workstation sends 11:00. A UTC
login node reads that as 13:00 CEST, an hour in the future, and reports `0
jobs`, so the administrator concludes that nothing failed. The MCP `query_slurm`
history uses the same code (`internal/mcpserver/read.go:342`), and no document
mentions time zones.

**Shown by:** The command was run at the same moment under different `TZ`
values. The fake ssh log showed `--starttime 2026-09-23T20:03:35` for `UTC`,
`2026-09-23T22:03:35` for `Europe/Berlin` and `2026-09-24T05:03:26` for
`Asia/Tokyo`, and each run printed `0 jobs`.

**Fix:** Send `--starttime now-<N>seconds`, or format the time in UTC and run
`sacct` with `TZ=UTC`.

### 12.5 `slurm user add` ignores `DEFAULT_ACCOUNT` for an existing user

**Medium** · `internal/slurm/slurm.go:512`

`AddUser` uses `DEFAULT_ACCOUNT` only when it creates a user. For a user who
already has associations, it runs `sacctmgr --immediate add user
account=<account> names=<user>` and drops the third argument, although the usage
line `add USER [ACCOUNT] [DEFAULT_ACCOUNT]` accepts it. The preview does not
mention the default account, and the command prints `user alice associated` and
exits 0. The user's jobs go on being charged to the old default account.

**Shown by:** With `sacctmgr` reporting `alice|other|other|1`, `clusterctl
--config site slurm user add alice proj proj -y` printed `user alice associated`
and exited 0. The fake ssh log shows `sacctmgr --immediate add user account=proj
names=alice`, with no `defaultaccount`.

**Fix:** When the user exists and `DEFAULT_ACCOUNT` is given, also run
`SetDefaultAccount` and show it in the preview, or refuse the argument for
existing users.

### 12.6 `sinfo`'s `none` reason is not recognised

**Medium** · `internal/mcpserver/plan.go:89` · `internal/cli/slurm.go:98`

The code treats a reason as absent only when it is empty or `(null)`. Real
`sinfo` prints neither for `%E`: `_print_reason` prints `none`, and the reason
user is `Unknown` (or `root` on a fresh node record). `slurm node list`
therefore shows `none [Unknown]` on every healthy node, where the guide shows an
empty `REASON`. The MCP resume plan warns `<node> was taken out with the reason
"none"` for every node. The one real reason the approving administrator should
read, such as a failing DIMM, is buried among dozens of false ones. The test
fixtures use `(null)`, which real `sinfo` does not produce.

**Shown by:** Given `sinfo` lines with `none` and `Unknown` plus one real
reason, the resume plan returned `exe0006 was taken out with the reason "none"`
next to `exe0007 was taken out with the reason "ticket 4711: failing DIMM"`.
`slurm node list` rendered `exe0001  idle  main  128  none [Unknown]`. The
`sinfo` behaviour was read from `src/sinfo/print.c`.

**Fix:** Treat `none`, `(null)` and the empty string as no reason, and drop the
reason user when there is no reason. Take the fixtures from real `sinfo` output.

### 12.7 `fabric state`, `fabric counters` and `fabric guid` misreport

**Medium** · `internal/cli/fabric.go:169` · `internal/cli/fabric.go:207` ·
`internal/cli/fabric.go:92`

`fabric state` reports a port `up` when the joined `ibportstate` lines contain
`Active` or `LinkUp`. The field names `LinkWidthActive` and `LinkSpeedActive`
always contain `Active`, so any port that `ibportstate` answers for is `up`,
whatever its `LinkState`. A port stuck in `Initialize` or `Armed` is therefore
reported healthy and the command exits 0, although `guides/reinstalling.md`
names `fabric state` as the check for "did its fabric link come up". A port that
is fully down usually gives no output and is still reported `down`.

`fabric counters --uplink` replaces the query with `ibqueryerrors --switch
--verbose --data --details --report-port`, which names no GUID, LID or port. It
dumps every switch port in the fabric instead of the one the node is cabled to.

`fabric guid` writes a node's lookup error into the `GUID` column, leaves the
node out of the `-o json` object and always exits 0, including when the DHCP
host cannot be reached. An MCP agent that reads it through `read_command` takes
the partial map as complete.

**Shown by:** A fake `ibportstate` reported `LinkState: Initialize` and
`PhysLinkState: LinkUp`, and `fabric state -n exe0001 -o wide` printed `up` and
exited 0. It still printed `up` with `LinkState: Down` and `PhysLinkState:
Polling`. The logged remote command for `fabric counters exe0001 --uplink`
carried no GUID, and `fabric guid -n 'exe[1-3]' -o json` printed only `exe0001`
and exited 0.

**Fix:** Parse the value of `LinkState`, report `up` only for `Active`, and show
`PhysLinkState` separately. For `--uplink`, resolve the peer switch port, for
example with `iblinkinfo`, and query only that port. In `fabric guid`, collect
the per-node errors, report them on stderr or under an error key, and return
`TargetFailed` or `Transport`.

### Lower severity

- **12.8 JSONPath, --dedup and node hw hide failures**
  (`internal/output/query.go:162`, `internal/cli/exec.go:175`,
  `internal/cli/node.go:387`). The JSONPath parser treats `range`, `end` and
  quoted literals as field names, so the kubectl idiom `{range
  .[*]}{.name}{"\n"}{end}` prints an empty line and exits 0, which reads as
  "nothing matched"; reject unsupported constructs explicitly. `--dedup` never
  prints a group's exit code, so a group that succeeded silently looks the same
  as one that failed silently, although for `grep -q` or `test -e` the status is
  the whole answer; output that differs only in trailing newlines is also
  merged, so show the status, and `unreachable`, in each group header. `node hw`
  labels every failed node `unreachable` whatever the cause and leaves it out of
  `-o json` and `-o yaml` entirely; emit an entry with the real error for each
  failed node.
- **12.9 Accounting changes and user checks** (`internal/slurm/slurm.go:467`,
  `internal/slurm/slurm.go:478`, `internal/cli/slurm.go:638`). `slurm account
  add`, `account shares`, `user add` and `user default` pass no `cluster=` to
  `sacctmgr`, so when several clusters share one slurmdbd, a change made from
  one context applies to all of them and the preview does not say so; add a
  `slurm.cluster` setting or read `ClusterName`, pass it to every `sacctmgr`
  call and show it in the preview. The user check runs `getent passwd`, which
  resolves local accounts and numeric UIDs through NSS just as `id` does, so the
  documented promise that "a local account on the login node is not mistaken for
  a directory user" is false; query only the directory source (`getent -s sss`),
  reject numeric names, or correct the documentation. The `user add` preview
  shows an empty account when `ACCOUNT` is omitted and leaves out the extra
  association that a different `DEFAULT_ACCOUNT` creates, and `sacctmgr` expands
  `,` and `[..]` in names, so `slurm account add 'proj[1-100]'` creates 100
  accounts under a preview that names one; resolve the defaults before the
  preview and reject those characters.
- **12.10 hca config and fabric GUIDs** (`internal/cli/fabric.go:347`,
  `internal/cli/fabric.go:382`, `internal/cli/fabric.go:37`). `hca config` takes
  a second argument without `[`, `@` or `,` as the value, so with
  `CLUSTERCTL_NODES` set, `hca config KEEP_LINK_UP_ON_BOOT_P1 exe0001` prepares
  a write of `KEEP_LINK_UP_ON_BOOT_P1=exe0001` to the whole session set; the
  gate shows this and only `-y` sends it, but make the write an explicit `hca
  config set KEY VALUE`. The write script changes only the first adapter that
  `ibstat -l` lists and still reports the node `ok`, whereas `hca firmware`
  loops over every adapter; loop here too, or report per device. `nodeGUIDs`
  uses the inventory MACs whenever there are any and asks DHCP only when there
  are none, which is the reverse of the documented order, so a stale inventory
  MAC makes `fabric guid` and `fabric state` ask about the wrong port; follow
  the documented order.
- **12.11 doctor misses checks it advertises** (`internal/cli/doctor.go:182`).
  `doctor --remote` always checks for `ipmipower` on the IPMI host, whatever
  `bmc.ipmi.backend` says: a site that uses `ipmitool` gets a false failure,
  `ipmitool` itself is never checked, and a configured absolute path is looked
  up by name in `PATH`. The age identity check uses `os.Stat` (`doctor.go:141`),
  so a path that names a directory passes as readable, and when the
  configuration fails to load, `doctor -o json` prints a table. Fix: check the
  binary the configured back end actually runs, at its configured path; open the
  identity files; and honour `-o` when the configuration fails.
- **12.12 DNS commands** (`internal/cli/dns.go:39`). `resolver` appends `:53`
  only when the server contains no `:`, so an IPv6 `services.dns.server` given
  without brackets or a port, such as `fd00::53`, fails every lookup with `too
  many colons in address`; the form `[fd00::53]:53` works. The Go resolver still
  answers from `/etc/hosts` before asking the configured server, and the help's
  promise that "a chain of aliases is shown as it is" is not kept: `LookupHost`
  returns addresses only, and `dns aliases` shows only the first PTR name. Fix:
  detect a port with `net.SplitHostPort`, and use `LookupCNAME` or a raw query
  to show alias chains.
- **12.13 Tab completion runs sinfo over ssh on every Tab**
  (`internal/cli/helpers.go:153`). Completion of `-n` and `completeGroups` call
  `Groups.List` for every source, and an `exec` source runs its command over ssh
  each time: only the map and all lookups are cached, whatever `cacheTtl` says.
  The request has no `Timeout` and the generated ssh configuration has no
  `BatchMode`, so a slow login node freezes the shell on every Tab, and ssh can
  prompt for a passphrase in the middle of a completion. Fix: cache `List`
  results for the source's `cacheTtl`, and in completion use only cached or
  local sources, or a short deadline with `BatchMode=yes`.

## 13. Release, CI and documentation

This section covers the release workflow, the version a binary reports, and
places where the documentation tells an administrator to do something the code
does not support. The first two issues decide what reaches administrators'
workstations as a trusted release.

### 13.1 The release signer allow-list is read from the commit being released

**Medium** · `.github/workflows/release.yml:46`

The `verify-tag` job checks out the tagged commit and trusts
`.github/allowed_signers` from that checkout, so the commit being released
supplies its own list of trusted signers. A commit that adds its signer's key to
the file passes the check, and a commit that deletes the file falls back to
checking only that a signature is present. The release job then builds the
commit and attests it with Sigstore. At ebed487 the file does not exist (only
`allowed_signers.example` does), so every release today takes the documented
presence-only path. The impact is limited: for a tag push GitHub runs
`release.yml` from the tagged commit, so a writer with the `workflow` permission
can remove the check outright. The flaw still matters for writers without that
permission, and it defeats the hardened mode that ADR 0007 and `doc/release.md`
promise: "with it, the signer is verified".

**Shown by:** In a scratch repository, the step ran verbatim. A tag signed by an
unlisted key failed with `No principal matched`, and it passed with
`::notice::v1.0.1 is signed by a listed signer` once the tagged commit added
that key to `.github/allowed_signers`. Deleting the file in the tagged commit
gave `::notice::v1.0.5 is signed; add .github/allowed_signers ...` and exit 0.

**Fix:** Read the signer list from a place the tagged commit cannot change, such
as a repository variable or `origin/main:.github/allowed_signers`, and fail when
it is missing. Protect `v*` tags with a ruleset and the release job with a
protected environment, which also guards against edits to the workflow.

### 13.2 The tag's own name is not checked against the pushed ref

**Medium** · `.github/workflows/release.yml:48`

`git verify-tag` checks the signature, but not that the `tag <name>` header
inside the signed object matches the ref it was pushed under, and the step adds
no such comparison. A maintainer's signed `v1.0.0` tag object pushed as
`refs/tags/v9.9.9` therefore passes. GoReleaser then publishes and attests the
old commit as `v9.9.9` and stamps that version into the binary. The old code
becomes the latest release, and mise and `releases/latest/download` serve it.
The attack needs repository write access, but no new commit and no `workflow`
permission. It contradicts the step's own comment that the tag must be "a
statement by a person holding a key rather than a label anyone with write access
can move".

**Shown by:** `git update-ref refs/tags/v9.9.9 $(git rev-parse
refs/tags/v1.0.0)` was followed by the verbatim step with
`GITHUB_REF_NAME=v9.9.9`. The step printed `::notice::v9.9.9 is signed by a
listed signer` and exited 0, while `git cat-file tag v9.9.9` showed `tag
v1.0.0`.

**Fix:** After verification, require `git cat-file tag "$tag" | sed -n 's/^tag
//p'` to equal `$GITHUB_REF_NAME`. Pass the verified tag object id to the
release job and check there that the ref still points at it.

### 13.3 The migration map sends `cluster-reboot-node` to `bmc power soft`

**Medium** · `doc/migration.md:84`

The command table maps the old reboot tool to `clusterctl bmc power soft`. The
help defines `soft` as "ask the operating system to shut down", and over Redfish
it sends `GracefulShutdown`, so the nodes power off and stay off. A graceful
reboot action does exist, `reboot` (`GracefulRestart`,
`internal/cli/bmc.go:257`), but it is not listed in the help or in
`ipmi.Actions()`. The confirmation gate and the Slurm check still apply, and the
nodes can be powered on again. Even so, an administrator who reboots a rack as
the guide says leaves it down.

**Shown by:** A trace of the code, with nothing executed. `doc/migration.md:84`
reads `` `clusterctl bmc power soft`, or `exec -- shutdown -r` ``,
`internal/cli/bmc.go:103` defines `soft`, and `resetTypeFor` maps it to
`redfish.ResetGracefulShutdown`.

**Fix:** Map the old command to `clusterctl bmc power reset` or to `exec --
shutdown -r`, or list `reboot` as a documented action and map the old command to
it.

### Lower severity

- **13.4 The signed-tag check is a text grep**
  (`.github/workflows/release.yml:41`). Without `.github/allowed_signers`, the
  only gate is an unanchored `grep -qE 'BEGIN (PGP|SSH) SIGNATURE'` over the
  whole tag object, message included, so an unsigned annotated tag whose message
  quotes that phrase is released as signed. This needs a forgotten `-s` and a
  message containing the marker, and it gives a deliberate attacker nothing the
  presence-only check does not already allow, but it breaks ADR 0007's claim
  that "an unsigned release is not possible by accident". Always run `git
  verify-tag` against a trusted signer list, and fail when there is none.
- **13.5 Release verify and test jobs hold a `contents: write` token**
  (`.github/workflows/release.yml:10`). `permissions: contents: write` is set at
  the top level, so `verify-tag` and `test` inherit it. `test` runs `make lint`
  and `go test ./...`, including dependency initialisation code, while the
  checkout keeps the token readable until the job ends. Set `contents: read` at
  the top level, grant write only in the `release` job, and use
  `persist-credentials: false` where nothing is pushed.
- **13.6 The `go` directive pins go1.26.0** (`go.mod:3`). With an older local Go
  and `GOTOOLCHAIN=auto`, a build in the checkout switches to go1.26.0, without
  the later standard-library fixes. `doc/release.md` documents that choice, but
  under go1.26.0 `make cover` fails with `go: no such tool "covdata"` because
  `internal/secrets/sopstest` has no test file, contrary to
  `doc/testing.md:68-69`. Add a test file to `internal/secrets/sopstest`, and
  consider a `toolchain` line that Dependabot keeps current.
- **13.7 Source builds do not report `devel (rev)`**
  (`internal/version/version.go:56`). `Get` prefers `bi.Main.Version`, which Go
  1.24 and later fill from VCS data. A checkout build therefore prints
  `v0.0.0-20260923180918-ebed4872343d (ebed4872343d) ...`, or the tag the commit
  carries, and `go install ...@vX` prints the module version with no revision.
  ADR 0007, `doc/release.md` and the install page of the manual all show `devel
  (a1b2c3d4e5f6) ...`. A pseudo-version is correct Go behaviour, so this is
  documentation drift: either ignore `bi.Main.Version` when no version was
  injected through `-ldflags` and report `devel` with the revision, or correct
  the three documents and the `Info.Version` comment.
- **13.8 Pages never publishes on release** (`.github/workflows/pages.yml:16`).
  GoReleaser creates the release with the workflow's `GITHUB_TOKEN`, and events
  caused by that token do not start other workflows. So `release: types:
  [published]` never fires, and the claim in `doc/release.md` that the site is
  published "on every release" does not hold. The impact is small because the
  tagged commit's push to `main` has normally published the site already; call
  the Pages workflow from `release.yml`, or correct the documentation.
- **13.9 README Get started points macOS users to the wrong directory**
  (`README.md:78`). On macOS, `config init` writes to `~/Library/Application
  Support/clusterctl` (from `os.UserConfigDir`). The README and the manual's
  getting-started configuration page tell the administrator to edit files under
  `~/.config/clusterctl`, where nothing is written and nothing is read. `config
  init` prints the full path of each file it writes, which limits the confusion;
  document the macOS path, or refer to the directory `config init` prints.

## How the review was done

1. **Area reviews.** Twelve reviewers each took one area:
   - the node set engine;
   - the ssh transport and host keys;
   - the core command path;
   - the MCP server;
   - service processors;
   - secrets;
   - configuration;
   - inventory, naming, groups and DHCP;
   - provisioning;
   - Slurm;
   - exec, tunnels, fabric, output and files;
   - build, release and CI.

   Each worked from the same threat model: output from nodes and BMCs, an MCP
   client, the shared configuration and the command line are untrusted or error
   prone. Each was asked to hold the code to the design notes, the decision
   records and the manual.
2. **Verification.** Every finding went to a separate verifier asked to refute
   it. The verifier checked the design notes and decision records for intent,
   and checked that the input can reach the code in the shipped binary. It then
   demonstrated the defect with a scratch test (through the `internal/cli`
   harness where possible) or with the built binary. Scratch tests were deleted
   afterwards.
3. **Gap hunts.** A completeness critic read what the reviewers had covered and
   set nine further hunts over two rounds:
   - protected-host aliases;
   - interrupts and cancellation;
   - secrets, again;
   - MCP read commands, one by one;
   - documentation promises nobody had checked;
   - the core command-line contract, again;
   - host lists passed to Slurm and FreeIPMI;
   - partial failure in batched changes;
   - overlapping `ssh_config` blocks.

   The first reviews of the core path and of secrets had not delivered their
   findings, so both areas were reviewed from scratch in these rounds.
4. **Challenge.** The 46 findings rated high or critical, and the one a
   verifier had left uncertain, went to an independent challenger. The
   challenger re-ran each reproduction and tried again to refute it. None was
   refuted. Several had their preconditions narrowed, and this report states the
   narrowed version. Medium and low findings had one verifier each.
5. **Result.** 255 findings were reported: 243 were confirmed and 12 refuted.
   The 243 were merged into the 162 issues above.

Limits:

- govulncheck could not reach `vuln.go.dev` from the review environment. No
  vulnerability scan was done, including of the go1.26.0 standard library that
  the `go` directive selects (see 13.6).
- Nothing ran against a real BMC, Slurm controller, PXE host or fabric. Those
  paths were exercised with the recording transport, fake `ssh` binaries and
  local TLS listeners. The behaviour of OpenSSH, Slurm and FreeIPMI was checked
  against their sources and, for OpenSSH, against real clients (8.9p1 and
  9.6p1).
- The review environment runs as root, so the multi-user side of 9.4 and 9.9
  was reasoned from the code rather than shown between two accounts.

## Refuted findings

These were reported, verified and found not to be defects, mostly because the
behaviour is the documented design. They are recorded so that they are not
reported again without new evidence.

- **`hostkey refresh` writes changed keys without showing the change**
  (`internal/cli/hostkey.go:217`). The help says refresh collects what each host
  offers and replaces what was recorded. The confirmation comes first by design.
- **`config init` without `DIR` writes into the user directory, which is read
  after `/etc/clusterctl`** (`internal/cli/config.go:133`). ADR 0017 chooses
  this on purpose. The related case, where this overrides a team configuration
  and removes its protected hosts, is kept as 9.8.
- **The confirmation preview does not name the context or cluster**
  (`internal/safety/safety.go:113`). `doc/safety.md` promises the action, the
  count and the hosts, and the preview prints exactly those.
- **`config validate` resolves only the current context**
  (`internal/cli/config.go:298`). Its help and its output both say so.
- **The drain and resume previews do not show the reason being replaced**
  (`internal/cli/slurm.go:184`). No document promises it in the terminal. The
  MCP plan shows the warning, as `doc/mcp.md` describes.
- **Actions are pinned to tags and some tools float to `latest`**
  (`.github/workflows/release.yml`). ADR 0012 records this choice.
- **The release is not gated on CI** (`.github/workflows/release.yml:58`).
  `doc/release.md` promises govulncheck in CI, which `ci.yml` runs, and does not
  promise a gate at tag time.
- **The `-- COMMAND` forms of `login`, `pdu shell` and the role shells run
  without the gate** (`internal/cli/bmc.go:873`). They are the documented
  equivalent of `ssh HOST CMD` on one host, and they honour `--dry-run`.
- **`secrets push` is not atomic across nodes**
  (`internal/cli/provision.go:125`). The promise is that every secret is
  decrypted before the first is written, and the code keeps it. The non-atomic
  write of a single file is kept as 8.8.
- **`hca cable` runs `mst cable add`** (`internal/cli/fabric.go:305`). That step
  is idempotent, is not persistent, and is needed before `mlxcables` can read a
  cable.
- **The gate decides from standard input whether it may ask, but prompts on
  standard error** (`internal/app/app.go:232`). The output guide documents this,
  and it is the usual convention.
- **A node whose name equals a role host inherits the role's ssh options**
  (`internal/transport/sshconfig.go:78`). `doc/transport.md` describes it, and
  it adds no exposure beyond the role itself. The case where it changes the
  login account is kept as 6.8.
