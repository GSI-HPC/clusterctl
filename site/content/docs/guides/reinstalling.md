---
title: Reinstalling nodes
weight: 4
---

A reinstall is four steps: forget the node's host keys, point the PXE service
at an installation, tell the machine to boot from the network once, and reset
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

$ clusterctl provision reinstall -n exe0007
About to reinstall 1 host: exe0007
  everything on these machines is lost
Continue? [y/N] y
STEP                          RESULT
forget the host keys          ok
configure the network boot    ok
boot from the network once    ok
reset the machines            ok

exe0007 is reinstalling; follow it with "clusterctl boot log"
```

{{< callout type="info" >}}
Everything is resolved before the first machine is touched. A set with one
node that has no boot path stops before anything changes, rather than leaving
half the set configured.
{{< /callout >}}

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
NODE     POWER  SSH  UPTIME
exe0007  On     no

$ clusterctl boot log
$ clusterctl dhcp log
```

## Doing it by hand

```console
$ clusterctl hostkey remove -n exe0007
$ clusterctl boot set -n exe0007
$ clusterctl bmc boot set Pxe -n exe0007
$ clusterctl bmc power reset -n exe0007
```

Useful flags: `--keep-host-keys` leaves the host key file alone,
`--no-reset` configures everything and lets you reset the machine yourself.

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
once all of it has arrived, so a dropped connection leaves the old file.

A node that cannot be reached is named, is not tried again for the remaining
secrets, and makes the push exit `3` when it is the only kind of failure.

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
```

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

## When a node does not come up

```console
$ clusterctl dhcp hosts -n exe0007       # does DHCP know it
$ clusterctl dhcp log                    # did it ask
$ clusterctl dhcp capture -i ib0         # is anything arriving at all
$ clusterctl boot status -n exe0007      # is a boot path set
$ clusterctl boot log                    # did the PXE service answer
$ clusterctl fabric state -n exe0007     # did its fabric link come up
```

`fabric state` works before the node has booted: the port is identified by the
adapter identifier derived from the hardware address DHCP knows. A port is up
only when its link state is `Active`; one that is physically linked but still
`Initialize` or `Armed` is reported down, with both states shown.
