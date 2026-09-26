---
title: Reinstalling nodes
weight: 4
---

A reinstall is four steps: point the PXE service at an installation, tell the
machine to boot from the network once, forget the node's host keys, and reset
it. `clusterctl provision reinstall` does all four, and each step is also a
command of its own so anything that goes wrong can be picked up by hand.

## Look before you touch

```console
$ clusterctl dhcp hosts -n exe0007
NODE     DECLARATION  MATCH      ADDRESS   MAC                BOOT FILE
exe0007  exe0007      name       10.0.2.7  aa:bb:cc:11:22:33  /srv/pxesrv/boot/exe/ipxe.net2
exe0007  exe0007-ib   interface  10.1.2.7  aa:bb:cc:11:22:44  /srv/pxesrv/boot/exe/ipxe.ib0

$ clusterctl boot status -n exe0007
NODE     ADDRESS   BOOT PATH
exe0007  10.0.2.7  none
```

The DHCP configuration is parsed, not grepped: a declaration whose options are
in an unusual order, or whose closing brace shares a line with a statement,
reports its own values, and `include` statements are followed. A construct the
parser does not understand stops the command instead of being guessed at.

A node without an address in the inventory boots with the address of the one
declaration named after it or its fully qualified name, the row whose `MATCH`
is `name`. A declaration for another interface (`interface`), such as
`exe0007-ib` or the BMC, never gives the boot address, and neither does one
whose comment names the node (`comment`), which is only shown. When several
declarations named after the node carry an address, or one hands out several,
`boot set` and `provision reinstall` refuse the node rather than pick one; set
its address in the inventory to settle it.

## Reinstalling

```console
$ clusterctl provision reinstall -n exe0007 --dry-run
Would reinstall 1 host: exe0007
  everything on these machines is lost
  /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2, for the next request: exe0007 (10.0.2.7)
  then each machine is set to boot from the network once and reset through Redfish

$ clusterctl provision reinstall -n exe0007
About to reinstall 1 host: exe0007
  everything on these machines is lost
  /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2, for the next request: exe0007 (10.0.2.7)
  then each machine is set to boot from the network once and reset through Redfish
Continue? [y/N] y
NODE     BOOT LINK  BOOT ONCE  RESET  STATE
exe0007  set        set        sent   reinstalling

exe0007 is reinstalling; follow it with "clusterctl provision status" and "clusterctl boot log"
```

The steps run in this order: write the boot link on the PXE host, set the
machine to boot from the network once, forget its host keys, and reset it.
The host keys go last, just before the reset, because they are the one change
that cannot be undone.

{{< callout type="info" >}}
Everything is resolved before the first machine is touched: each node's
address, boot path, service processor and BMC credential. The boot paths are
checked on the PXE host, and Slurm is asked whether the nodes run jobs. A set
with one node that fails any of these stops before anything changes, rather
than leaving half the set configured. These checks only read, so a dry run
makes them too and is refused where the real run would be.
{{< /callout >}}

Slurm is asked the way `bmc power reset` asks it: a node that runs a job, or
that Slurm cannot say about, is refused unless `--lose-jobs` is given, and a
dry run asks too. `--force` gets past a protected host, not this check. With
`--no-reset` nothing is reset, so Slurm is not asked.

The boot override is set over Redfish. A node whose `bmc.order` puts IPMI
first is refused; reinstall it step by step, as below.

### When a step fails

If a step fails, every node that was not reset is disarmed: its boot override
is cleared and its boot link removed, so it does not reinstall at some later
boot nobody confirmed. The table shows what became of each step on each node,
and the error says which nodes are reinstalling, which were disarmed, and
which could not be, with the commands that finish the job:

```console
$ clusterctl provision reinstall -n 'exe[0001-0003]'
...
NODE     BOOT LINK  BOOT ONCE    RESET  STATE
exe0001  removed    set          -      armed
exe0002  removed    unreachable  -      disarmed
exe0003  removed    cleared      -      disarmed

clusterctl: setting the machines to boot from the network once failed:
exe0002: exe0002.mgmt.hpc.example.org: dial tcp: connection refused; the boot
links and boot overrides of exe[0002-0003] were removed again; exe0001 is left
armed and reinstalls at its next network boot;
disarm it with "clusterctl bmc boot unset -n exe0001"
```

A service processor that cannot be reached exits `3`, a missing credential
`2`, and a processor that refused `1`.

The boot path comes from the cluster rules unless `--boot-path` names one:

```yaml
bootPaths:
  - nodes: exe[0001-1024]
    path: /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2
  - nodes: wlm01
    path: /srv/pxesrv/boot/cluster/1.0/wlm01/ipxe.net2
```

A node matched by two rules is an error, not a silent first match.

## Watching it

```console
$ clusterctl provision status -n exe0007
NODE     BOOT PATH                                   POWER  SSH  UPTIME  ERROR
exe0007  /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2  On     no

$ clusterctl boot log
$ clusterctl dhcp log
```

The boot path is the node's link on the PXE service, which the installation
consumes, so `none` once the machine has fetched it. The service processors
and the nodes are asked at the same time, so processors that do not answer do
not hold back the answers over ssh. A node that does not answer over ssh yet
is not an error. A boot path or power state that cannot
be read is, and fails the command with exit code `3` when the host could not
be reached; the JSON output lists every node, with an `error` field for the
ones that failed.

## Doing it by hand

```console
$ clusterctl boot set -n exe0007
$ clusterctl bmc boot set Pxe -n exe0007
$ clusterctl hostkey remove -n exe0007
$ clusterctl bmc power reset -n exe0007
```

Useful flags: `--keep-host-keys` leaves the host key file alone,
`--no-reset` configures everything and lets you reset the machine yourself.
A set left armed that way reinstalls at its next network boot; `clusterctl bmc
boot unset` and `clusterctl boot unset` disarm it.

## Afterwards

```console
$ clusterctl hostkey refresh -n exe0007
$ clusterctl secrets push -n exe0007
$ clusterctl cinc config http://installer/cinc/latest.tgz -n exe0007 --run-list 'role[exe]'
$ clusterctl cinc run -n exe0007
$ clusterctl slurm node resume -n exe0007
```

### Secrets

```console
$ clusterctl secrets list
SOURCE                                    TARGET                MODE  OWNER
secret example/munge-key                  /etc/munge/munge.key  0400  munge:munge
/etc/clusterctl/secrets/nslcd.keytab.age  /etc/nslcd.keytab     0600

$ clusterctl secrets push -n exe0007
```

The plaintext is decrypted into memory on your workstation and streamed to the
node over standard input. It never lands on either disk, and it never appears
in an argument vector. Every secret is decrypted before you are asked, so a
key you lack stops the push, and its `--dry-run`, before the node is touched.
On the node, each file is written beside its target and moved into place only
once all of it has arrived, so a dropped connection leaves the old file. Two
secrets with the same target are refused before anything is decrypted: only
the last would stay, after the first had been in place for a while.

The nodes are written to side by side, `fanout.max` at a time, each its
secrets one after the other, so a node that hangs holds up no other. A node
that cannot be reached is named, is not tried again for the remaining
secrets, and makes the push exit `3`, even when another node refused.

A secret can come from a sops encrypted `Secret` document instead of a file of
its own:

```yaml
      secrets:
        - target: /etc/munge/munge.key
          mode: "0400"
          secretRef: {name: example, key: munge-key}
```

Before a reinstall that needs them, check that you can open every one:

```console
$ clusterctl secrets check --decrypt
SECRET   FILE                               KEYS  ENCRYPTED TO  USED  STATUS
example  /etc/clusterctl/secrets.sops.yaml  2     3 age         2     decrypts

1 Secret documents, decrypted with sops 3.13.3 at /usr/bin/sops
```

The `sops` command decrypts them, so it has to be installed on your
workstation; the caption names the one that did.

### Configuration management

`cinc config` writes the archive URL, and the run list if you give one, into
`/etc/cinc/solo` on each node (`services.cinc.soloConfigPath` moves it). The
URL must be an `http` or `https` URL. Each value is written quoted, and the new
file replaces the old one only once it has arrived complete. It replaces it
whole: leave out `--run-list` and a run list written earlier is gone, and the
client falls back to the run list of the archive.

`cinc run` reads that file without sourcing it and hands the two values to the
client as arguments; any other assignment in the file is ignored. A node whose file holds anything but plain assignments,
such as an unquoted `$(...)` left by hand or by an older tool, is refused and
the client is not started there; run `cinc config` again to rewrite it.
The client may run for 30 minutes on each node, or for `fanout.commandTimeout`
when that is longer, since converging a freshly installed node takes longer
than most commands.

```console
$ clusterctl cinc show -n exe[0007-0008]
NODE     ARCHIVE                                                         RUN LIST
exe0007  http://installer/cinc/latest.tgz                                role[exe]
exe0008  failed: ssh: connect to host exe0008 port 22: No route to host
```

`cinc show` lists a node without the file as `not configured`. A node it could
not read is shown as failed and makes the command fail, with exit code `3`
when the node could not be reached.

## Boot configurations

```console
$ clusterctl boot list
$ clusterctl boot sync            # git pull on the PXE host, after a question
```

`boot set` resolves every address and checks every boot path on the PXE host
before it asks, and the question lists each path with its nodes:

```console
$ clusterctl boot set -n exe0007
About to set the network boot configuration of 1 host: exe0007
  /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2, for the next request: exe0007 (10.0.2.7)
Continue? [y/N] y
NODE     ADDRESS   BOOT PATH                                   MODE  RESULT
exe0007  10.0.2.7  /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2  once  set
```

Every node is tried and listed, and a node whose link could not be written
fails the command, so a rerun, or `boot unset`, knows what is left.

A persistent boot path, with `--persistent` or from a rule marked
`static: true`, survives the first request. It is the link named with
`services.pxesrv.staticSuffix`, and it is refused when no suffix is set.
`boot status` then shows both links, and `boot unset` removes both:

```console
$ clusterctl boot status -n exe0007
NODE     ADDRESS   BOOT PATH  PERSISTENT
exe0007  10.0.2.7  none       none

$ clusterctl boot unset -n exe0007
```

## GRUB over TFTP

Where nodes load a GRUB configuration named after their address in
hexadecimal:

```console
$ clusterctl boot grub show exe0007
NODE     ADDRESS   GRUB FILE
exe0007  10.0.2.7  grub.cfg-0A000207

$ clusterctl boot grub set exe0007 /srv/tftp/grub/1.0/grub.cfg.install-exec
$ clusterctl boot grub unset exe0007
```

A GRUB link has no one-shot form: the node loads its target at every boot
until `boot grub unset` removes the link.

The target is a file on the TFTP host, relative to `services.tftp.grubPath`
unless it is absolute. It has to exist and to lie under `services.tftp.root`,
which is all the TFTP server serves; anything else exits `2` before the link
is touched, `--dry-run` included. The link is written relative to its
directory, as `1.0/grub.cfg.install-exec` above, so that a TFTP server
confined to its root follows it too.

`boot grub log` shows what the TFTP server wrote into
`services.tftp.logPath`, `/var/log/syslog` unless configured: whether the node
asked for its file, and what it was given.

## When a node does not come up

```console
$ clusterctl dhcp hosts -n exe0007       # does DHCP know it
$ clusterctl dhcp log                    # did it ask
$ clusterctl dhcp capture -i ib0         # is anything arriving at all
$ clusterctl boot status -n exe0007      # is a boot path set
$ clusterctl boot log                    # did the PXE service answer
$ clusterctl boot grub log               # did TFTP serve its GRUB file
$ clusterctl fabric state -n exe0007     # did its fabric link come up
```

`fabric state` works before the node has booted: the port is identified by the
adapter identifier derived from the hardware address DHCP knows. A port is up
only when its link state is `Active`; one that is physically linked but still
`Initialize` or `Armed` is reported down, with both states shown. The fabric
host asks about four ports at a time, in one session. When it stops before it
has answered for every port, at the command's timeout or because the
connection dropped, the ports it answered for are still shown, the others
read `no answer`, and the command fails with the reason. `--dry-run` asks the
fabric too, since `fabric state` only reads.
