---
title: Power and service processors
weight: 3
---

Service processors are reached out of band, so a machine that will not boot can
still be talked to.

Redfish is tried first, because IPMI over LAN ships disabled on current iLO,
XCC and iDRAC firmware and trying it first only produces timeouts. Where IPMI
is used, the tools run on the host with a route into the management network and
the password never reaches an argument vector.

## Reading state

```console
$ clusterctl bmc status -n '@rack:R02'
NODE     STATE  ERROR
exe0001  On
exe0002  On
exe0003  Off
exe0004         exe0004.mgmt.hpc.example.org: dial tcp: i/o timeout

$ clusterctl bmc redfish info -n exe0001 -o wide
NODE     POWER  HEALTH  MODEL           BIOS  RESET TYPES
exe0001  On     OK      Vendor Model X  2.1   On,ForceOff,ForceRestart,GracefulRestart,PowerCycle
```

`reset types` is what that firmware actually accepts. It is worth looking at
once per hardware generation.

## Changing state

```console
$ clusterctl bmc power soft  -n exe0003     # ask the OS to shut down
$ clusterctl bmc power off   -n exe0003     # cut the power
$ clusterctl bmc power on    -n '@rack:R02'
$ clusterctl bmc power cycle -n exe0003
$ clusterctl bmc power reset -n exe0003     # without asking the OS
```

Every one of these previews and asks:

```console
$ clusterctl bmc power off -n '@rack:R02'
About to power off 10 hosts: exe[0001-0010]
  through Redfish
This is more than 8 hosts. Type the number of hosts to continue: 10
```

{{< callout type="warning" >}}
A node running a Slurm job is refused, because powering it off loses the job.
A node that is still `draining` runs jobs too. Drain it and wait for its jobs
to end, or pass `--lose-jobs` if losing them is the intention. `--force` gets
past a protected host, not this check.
{{< /callout >}}

```console
$ clusterctl bmc power off -n exe0007
clusterctl: exe0007 is running Slurm jobs; drain them and wait for their jobs to end, or pass --lose-jobs to lose the jobs
```

The check refuses whatever it cannot vouch for: a state it does not know, a
node `sinfo` does not list, and every node when Slurm cannot be asked. A node
whose Slurm name differs from its inventory name is refused this way:

```console
$ clusterctl bmc power off -n exe0007
clusterctl: Slurm did not report exe0007, so whether it runs jobs is not known (check that the Slurm and inventory names agree); pass --lose-jobs to go ahead and lose any jobs on them
```

`safety.slurmAware` turns the check on or off and is on by default. With it
off, every power action says so before it asks. `--dry-run` asks Slurm as
well, so a dry run is refused where the real run would be.

### Powering on a rack

A rack powering on at once trips its breaker, so a power-on is sent in batches:

```console
$ clusterctl bmc power on -n '@rack:R02' -y
powering on exe[0001-0008] (1 of 2)
waiting 5s before the next batch
powering on exe[0009-0010] (2 of 2)
```

`safety.powerOnBatch` and `safety.powerOnStagger` configure it;
`--batch` and `--stagger` override for one command.

### A reset is sent once

A reset that timed out may already have been carried out, so nothing retries
it: sending it twice would power cycle a running machine. If a reset times out,
read the power state and decide.

### An unsupported reset type is refused before it is sent

```console
$ clusterctl bmc power soft -n exe0001
clusterctl: exe0001.mgmt...: does not accept the reset type "GracefulShutdown";
  it accepts ForceOff, ForceRestart, GracefulRestart, On, PowerCycle
```

The machine is asked what it accepts rather than guessed at.

## What a machine boots next

```console
$ clusterctl bmc boot show -n exe0001
NODE     SOURCE  MODE      ERROR
exe0001  None    Disabled

$ clusterctl bmc boot set Pxe -n exe0001
$ clusterctl bmc boot unset -n exe0001
```

{{< callout type="warning" >}}
An override applies **once** by default. `--persistent` keeps it until it is
removed, which is how a machine ends up reinstalling every time it reboots.
{{< /callout >}}

## Certificates

A service processor carries a self signed certificate that no authority
vouches for. clusterctl records the one it sees first and refuses a change,
the way ssh does with host keys. It says so when it is about to record one,
so that a first contact you did not expect is noticed:

```console
$ clusterctl bmc status -n exe0042
no certificate is recorded for exe0042.mgmt.hpc.example.org yet; the one it presents now will be recorded and trusted from then on
```

The certificate is recorded under the host clusterctl talks to: the
`bmcAddress` of the node when the inventory sets one, otherwise the name the
naming rules give.

```console
$ clusterctl bmc status -n exe0001
clusterctl: the certificate of exe0001.mgmt.hpc.example.org changed:
  recorded sha256:1a2b…, now sha256:9f8e….
  If the certificate was replaced on purpose, run "clusterctl bmc forget exe0001" and try again
```

```console
$ clusterctl bmc forget exe0001
```

Do that after replacing a certificate on purpose. If it changed and nobody
replaced it, find out why first.

## Talking to Redfish directly

```console
$ clusterctl bmc redfish get /redfish/v1/Systems/1 -n exe0001
$ clusterctl bmc redfish get /redfish/v1/Managers -n exe0001 \
    -o jsonpath='{.exe0001.Members[*]}'
$ clusterctl bmc redfish post /redfish/v1/Systems/1/Actions/ComputerSystem.Reset \
    '{"ResetType":"ForceRestart"}' -n exe0001
```

`post` can power off a machine, so it goes through the same confirmation as any
other destructive command.

## Firmware that differs

```yaml
bmc:
  vendors:
    vendor2:
      resetTypes: [On, ForceOff, ForceRestart, GracefulRestart, PowerCycle]
      systemPath: /redfish/v1/Systems/Self
      minTlsVersion: "1.0"
```

Keyed by the node's `vendor` attribute in the inventory. This is how a
generation of hardware with unusual firmware is handled without a code change.

## Rack power

```console
$ clusterctl pdu list
RACK  PDU                       NODES
R01   pdu0-R01.mgmt.example.org  dbm01,wlm01
R02   pdu0-R02.mgmt.example.org  exe[0001-0010]

$ clusterctl pdu shell 1 R02
$ clusterctl pdu shell 1 R02 -- show outlets
```

## Which processors answer

```console
$ clusterctl bmc ping -n '@rack:R02'
BMC                           STATE
exe0001.mgmt.hpc.example.org  alive
exe0004.mgmt.hpc.example.org  no answer

9 of 10 answered
```

One sweep from the gateway, not one connection per processor.
