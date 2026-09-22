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

## The generated ssh configuration

Every connection is made with a configuration clusterctl writes into
`$XDG_STATE_HOME/clusterctl/ssh_config`. Read it when something surprises you:

```console
$ cat ~/.local/state/clusterctl/ssh_config
```

It puts what clusterctl needs first — ssh keeps the first value it obtains —
and includes the system configuration afterwards, so distribution crypto
policies, GSSAPI and your own `Host` blocks keep applying to everything it does
not set.

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
