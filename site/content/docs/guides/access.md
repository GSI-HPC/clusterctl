---
title: Access and trust
weight: 6
---

## The host key file

One file is the site's trust anchor. Keep it in version control next to the
site configuration, and name it in the `Site` document:

```yaml
ssh:
  knownHostsFile: ssh-known-hosts
```

Every connection is checked against it.

```console
$ clusterctl hostkey list
$ clusterctl hostkey scan -n exe0007        # look, change nothing
$ clusterctl hostkey verify -n '@compute'   # compare with the file
$ clusterctl hostkey refresh -n exe0007     # write what the hosts offer
$ clusterctl hostkey remove -n exe0007      # before a reinstall
```

`verify` exits non-zero when a key changed:

```console
$ clusterctl hostkey verify -n '@compute'
HOST                          STATUS   DETAIL
exe0001.hpc.example.org       matches  ssh-ed25519
exe0007.hpc.example.org       CHANGED  file has AAAAC3Nza…, host offers AAAAC3Nza…
clusterctl: 1 host key changed; check before refreshing
```

{{< callout type="warning" >}}
A changed key is either a machine that was reinstalled or something worth
investigating. clusterctl will not decide which — that is why `verify` and
`refresh` are separate commands.
{{< /callout >}}

Keys are collected by starting an SSH handshake and abandoning it the moment
the server presents its key, so no credentials are involved. The strongest key
the host has is taken: Ed25519, then ECDSA, then RSA, and for an sshd older
than OpenSSH 7.2 the same RSA key over `ssh-rsa`. A host that serves a role
with a `proxyJump` is reached through its jump host with `ssh -W`, over the
generated configuration, so the jump host's own key is checked against the
file first; that connection runs in batch mode and never prompts. Hosts are
scanned in parallel up to `fanout.max`, and `--timeout` bounds each host once,
from the connection to the key. With `--bmc`, `scan`, `verify`, `refresh` and `remove` act on
the nodes' service processors instead, reached the way the `bmc` commands
reach them: at the `bmcAddress` the inventory records, else by the name the
naming rules give them, and the key is recorded under that host. The file is always
rewritten completely, under a lock, and sorted, so two administrators
refreshing at once cannot lose an entry and the diff is readable.

To share the file, keep it in a directory of its own that its group can write,
with the setgid bit set: `chmod 2775`, and name it as
`knownHostsFile: hostkeys/ssh-known-hosts`. Not the configuration directory
itself: clusterctl refuses configuration others can write. clusterctl keeps the file's mode and group when it
rewrites it, and its lock readable by the group, so every administrator in the
group can refresh it. A symbolic link to the file stays a link, unless someone
other than you or root made it, in which case the write is refused.

The rewrite keeps what people wrote into the file. A comment above an entry
stays with that entry when the file is sorted, and `refresh` carries it over
to the key that replaces it; the comment block at the top stays on top. Lines
ssh itself understands are read the way ssh reads them:

- A hashed name (`|1|…`) or a wildcard (`*.mgmt.example.org`, `!exe0009…`)
  covers the hosts ssh would match it with, so `verify` does not report them
  as missing. `remove` and `refresh` only drop a line that names the host
  itself, literally or hashed; a wildcard speaks for other hosts too and stays.
- `@revoked` and `@cert-authority` lines are never removed or replaced. A host
  offering a revoked key is reported as `REVOKED` by `verify`, and `refresh`
  refuses to write that key; both exit non-zero.

## The generated ssh configuration

Every connection is made with a configuration clusterctl writes into
`$XDG_STATE_HOME/clusterctl`. Each configuration gets a file of its own, named
by a digest of its content, so two contexts or two runs with different
settings never share one. `login --dry-run` names the file; read it when
something surprises you:

```console
$ clusterctl login --dry-run login
ssh -F ~/.local/state/clusterctl/ssh_config-3f9c2a1b7d4e5f60 -- alice_adm@login.hpc.example.org
$ cat ~/.local/state/clusterctl/ssh_config-3f9c2a1b7d4e5f60
```

It puts what clusterctl needs first — ssh keeps the first value it obtains —
and includes your own `~/.ssh/config` and the system configuration afterwards,
so distribution crypto policies, GSSAPI and your own `Host` blocks keep
applying to everything it does not set. `ssh.include` lists the files; a
relative entry is read from the site's directory.

Two things no included file can change. Host keys are checked against the
site's file alone: the global known hosts files, a `KnownHostsCommand` such as
the one a FreeIPA client installs, and DNS records are all switched off. And no
connection goes through a multiplexing master unless its role asks for one, so
a `ControlMaster` in your own configuration does not reach the compute nodes.

Per-role settings come from the `Site` document:

```yaml
hosts:
  mgmt:
    host: mgmt-gw.example.org
    forwardAgent: true
    controlMaster: true        # a gateway is worth a shared connection
  dhcp:
    host: dhcp01.example.org
    user: root
    proxyJump: mgmt            # a role name, or a host
    legacyAlgorithms: true     # an sshd too old for current defaults
    options:
      IdentityFile: ~/.ssh/id_infra
```

{{< callout type="info" >}}
`controlMaster` is for gateways and hubs, never for compute nodes: one
multiplexed connection per node runs into sshd's `MaxStartups` at scale.
{{< /callout >}}

Jump hosts are configuration, not a flag. A role that needs one says so, and
every command then uses it — which is why `clusterctl login -J` is refused.
`proxyJump` takes a role name, optionally with an account as in `admin@mgmt`,
or a fully qualified host, and a comma separated list of either. A bare word
that is not a role is refused as a likely misspelling, and so is a chain that
comes back to where it started.

A role's account is passed on the command line, not written into its block, so
a node whose host name is also a role's host still logs in with your own
account.

`options` takes extra ssh_config keywords. The ones that decide which host keys
are trusted, `Host`, `Match` and `Include`, and the ones clusterctl already
writes, are refused with a message naming the setting to use instead: ssh
would otherwise obey them over the site's trust settings, or silently ignore
them. `config validate` reports all of this, and every command refuses to run
until it is fixed.

## Tunnels

Where a network is only reachable through a gateway, a tunnel routes it:

```yaml
tunnels:
  ipmi:
    remote: mgmt
    subnets: [ipmi]                   # a network name, or a CIDR
    excludes: ["{workstation.host}"]  # keep this machine off the tunnel
    description: Reach the service processors directly
```

```console
$ clusterctl tunnel list
$ clusterctl tunnel start ipmi
$ clusterctl tunnel status
NAME      STATE  PID     REMOTE
ipmi      up     123456  mgmt
internal  down           pool
$ clusterctl tunnel stop ipmi
```

sshuttle changes the local firewall, so starting one may ask for your local
password.

A tunnel connects the way every other connection does: sshuttle is handed
`ssh -F` and the generated configuration, so the gateway's host key is checked
against the site's file alone, and the role's `proxyJump` and `options` apply.
The account is the profile's `user`, else the role's, else the context's.

```console
$ clusterctl tunnel start ipmi --dry-run
sshuttle --exclude desk01.example.org --daemon --pidfile … --ssh-cmd 'ssh -F …/ssh_config-3f9c2a1b7d4e5f60' --remote alice_adm@mgmt-gw.example.org 10.0.0.0/8
```

The profile's `options` come before the options clusterctl sets, which
sshuttle then keeps. An option that would set one of those itself,
`--ssh-cmd`, `--remote`, `--pidfile` or `--daemon`, is refused.

A tunnel is found by its process id file, and a process counts as the tunnel
only while it runs with that very file: a file left behind by a crash, even
one whose number now belongs to another of your processes, is not reported as
a running tunnel, and `tunnel stop` signals nothing and removes the file.
`tunnel stop` takes only the name of a configured profile.

An exclude that expands to nothing is refused rather than dropped:
`{workstation.host}` is empty on a machine no `Workstation` document
describes, and dropping the exclude would route this machine's own address
into the tunnel.

A subnet that is neither a configured network name nor an address is refused,
because sshuttle would otherwise route something else entirely.

## Protected hosts

```yaml
safety:
  protectedHosts:
    - wlm01
    - dbm01
    - "@inventory:infra"
  confirmAbove: 8
  slurmAware: true
```

A destructive command touching one of these stops and names it:

```console
$ clusterctl bmc power off -n 'exe[1-4],wlm01'
clusterctl: power off would touch the protected host wlm01; pass --force to do it anyway
```

The protected hosts are compared by machine, not by spelling. `WLM01`,
`wlm01.`, `wlm01.hpc.example.org`, its service processor
`wlm01.mgmt.hpc.example.org` and its inventory address `10.0.1.1` are all
wlm01, and are refused as it is. An entry may name a machine in any of these
ways, or name a group, but every machine it names has to be in the inventory:

```console
$ clusterctl config validate
clusterctl: safety.protectedHosts[0] "wlm02": it names wlm02, which the inventory does not know; write the inventory name of the machine
```

An entry with a group is resolved when a command is about to change
something, and by `config validate`. If its source cannot be asked, every
change is refused until it can, or until `--force` is given.

A change to a node the inventory does not know is refused too, because the
gate cannot tell whether it is another name for a protected host. Pass
`--force` when you mean it.

`--force` never lets a host through silently. The preview names the protected
hosts and the unknown nodes it lets through, before the question and in a dry
run, and with `-y`, or for `exec` without `--confirm`, the same lines go to
standard error:

```console
$ clusterctl bmc power off --force -y -n 'exe[1-4],wlm01'
--force lets through the protected host wlm01
```

`confirmAbove` is the host count above which the number has to be typed back
rather than confirmed with a `y`. At `0` the number is asked for every time.
