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
NODE     DECLARATION  ADDRESS   MAC                BOOT FILE
exe0007  exe0007      10.0.2.7  aa:bb:cc:11:22:33  /srv/pxesrv/boot/exe/ipxe.net2
exe0007  exe0007-ib   10.1.2.7  aa:bb:cc:11:22:44  /srv/pxesrv/boot/exe/ipxe.ib0

$ clusterctl boot status -n exe0007
NODE     ADDRESS   BOOT PATH
exe0007  10.0.2.7  none
```

The DHCP configuration is parsed, not grepped, so a declaration whose options
are in an unusual order reports its own values.

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
SOURCE                                TARGET               MODE  OWNER
/etc/clusterctl/secrets/munge.key.age /etc/munge/munge.key 0400  munge:munge
/etc/clusterctl/secrets/nslcd.keytab.age /etc/nslcd.keytab 0600

$ clusterctl secrets push -n exe0007
```

The plaintext is decrypted into memory on your workstation and streamed to the
node over standard input. It never lands on either disk, and it never appears
in an argument vector.

## Boot configurations

```console
$ clusterctl boot list
$ clusterctl boot sync            # git pull on the PXE host
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
```

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
adapter identifier derived from the hardware address DHCP knows.
