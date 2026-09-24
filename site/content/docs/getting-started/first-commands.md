---
title: First commands
weight: 3
---

Everything here is read-only. Run it in order the first time you point
clusterctl at a site.

## Does the configuration resolve

```console
$ clusterctl config validate
FILE                        KIND           NAME
/etc/clusterctl/cluster.yaml  Cluster      cluster1
/etc/clusterctl/config.yaml   Config
/etc/clusterctl/inventory.yaml NodeInventory example
/etc/clusterctl/site.yaml     Site         example

4 documents are valid; context cluster1 resolves
```

A mistake is reported where you wrote it:

```console
$ clusterctl config validate
clusterctl: site.yaml is not valid:
  site.yaml:9:7: spec.hosts.login.forwardAgnet: unknown field "forwardAgnet"; did you mean "forwardAgent"?
```

## Can it reach things

```console
$ clusterctl doctor
CHECK                        STATUS   DETAIL
configuration                ok       context cluster1, cluster cluster1, site example
ssh client                   ok       /usr/bin/ssh OpenSSH_9.6p1
generated ssh configuration  ok       ~/.local/state/clusterctl/ssh_config
host key file                ok       412 entries in /etc/clusterctl/ssh-known-hosts
node inventory               ok       1024 nodes

$ clusterctl doctor --remote
```

`--remote` contacts each configured host role and checks the programs the
commands need are there.

## Do node sets resolve

```console
$ clusterctl node select '@compute'
exe[0001-1024]

$ clusterctl node select '@compute!@drained' --count
1019

$ clusterctl node groups
SOURCE     GROUP
inventory  exe
inventory  sub
rack       R01
slurm      main
```

## What is known about a node

```console
$ clusterctl node describe exe0007
FIELD             VALUE
name              exe0007
host              exe0007.hpc.example.org
bmc               exe0007.mgmt.hpc.example.org
address           10.0.2.7
rack              R02
attribute.class   exe
attribute.vendor  vendor2
groups.inventory  exe
groups.rack       R02
```

## Can it reach a host

```console
$ clusterctl login --dry-run login
ssh -F ~/.local/state/clusterctl/ssh_config -- alice_adm@login.hpc.example.org

$ clusterctl login login -- uptime
 14:42:30 up 31 days,  4:10,  0 users,  load average: 12.04, 11.87, 11.90
```

## Can it reach the nodes

```console
$ clusterctl exec -n 'exe[1-4]' -- uptime -p
exe0001: up 3 weeks, 1 day
exe0002: up 3 weeks, 1 day
exe0003: up 3 weeks, 1 day
exe0004: up 2 days

$ clusterctl exec -n '@compute' --dedup -- uname -r
exe[0001-1020] (1020)
  5.14.0-570.el9.x86_64
exe[1021-1024] (4)
  5.14.0-503.el9.x86_64
```

## What does the hardware say

```console
$ clusterctl bmc status -n 'exe[1-4]'
NODE     STATE  ERROR
exe0001  On
exe0002  On
exe0003  Off
exe0004  On
```

If this is the first time clusterctl has spoken to those service processors,
it records the certificate each one presents. A later change is refused until
you say the replacement was intentional with `clusterctl bmc forget`.

## Next

{{< cards >}}
  {{< card link="../guides/node-sets" title="Selecting nodes" >}}
  {{< card link="../guides/running-commands" title="Running commands" >}}
  {{< card link="../guides/power" title="Power and service processors" >}}
{{< /cards >}}
