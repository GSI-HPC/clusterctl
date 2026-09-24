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
the server presents its key, so no credentials are involved. The file is always
rewritten completely, under a lock, and sorted, so two administrators
refreshing at once cannot lose an entry and the diff is readable.

To share the file, keep it in a directory of its own that its group can write,
with the setgid bit set: `chmod 2775`, and name it as
`knownHostsFile: hostkeys/ssh-known-hosts`. Not the configuration directory
itself: clusterctl refuses configuration others can write. clusterctl keeps the file's mode and group when it
rewrites it, and its lock readable by the group, so every administrator in the
group can refresh it. A symbolic link to the file stays a link, unless someone
other than you or root made it, in which case the write is refused.

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
password. A process id file left behind by a crash is not reported as a running
tunnel — the process is checked too.

```console
$ clusterctl tunnel start ipmi --dry-run
sshuttle --daemon --pidfile … --remote mgmt-gw.example.org --exclude desk01.example.org 10.0.0.0/8
```

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

`confirmAbove` is the host count above which the number has to be typed back
rather than confirmed with a `y`.
