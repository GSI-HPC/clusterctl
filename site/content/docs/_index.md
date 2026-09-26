---
title: Manual
weight: 1
cascade:
  type: docs
---

clusterctl administers one or more HPC clusters from an administrator's
workstation. It replaces a toolkit of shell scripts with a single binary, one
layered configuration and a command line that means the same thing everywhere.

## Where to start

{{< cards >}}
  {{< card link="getting-started" title="Getting started" subtitle="Install it, write a configuration, run the first commands." >}}
  {{< card link="guides" title="Guides" subtitle="How to do each job: select nodes, fan out, power, reinstall, Slurm." >}}
  {{< card link="reference" title="Reference" subtitle="Configuration fields, exit codes, environment variables." >}}
  {{< card link="../reference" title="Command reference" subtitle="Every command and flag, generated from the binary." >}}
{{< /cards >}}

## The shape of a command

Every command is a noun then a verb, and the global flags mean the same thing
in all of them:

```console
$ clusterctl [--context C] [-n NODESET] [-o FORMAT] [--dry-run] [-y] NOUN VERB
```

| Flag | Meaning |
| --- | --- |
| `-n`, `--nodes` | The node set to act on |
| `-o`, `--output` | `table`, `wide`, `json`, `yaml`, `nodeset`, `name`, `jq=…` |
| `--context` | Which cluster to act on |
| `--dry-run` | Say what would happen and change nothing |
| `-y`, `--yes` | Answer the confirmations in advance |
| `--force` | Allow a protected host, or a node the inventory does not know, to be touched |
| `--set` | Override one configuration value for this command |
| `--progress` | `auto`, `counter`, `plain` or `none`: how progress is shown on standard error |

## A note on safety

Anything that powers off, reinstalls, drains or overwrites shows you what it is
about to do and asks. Above a configured host count it asks you to type the
number of hosts, because a `y` is easy to type by reflex. Without a terminal to
ask on, it refuses rather than proceeding — pass `-y` when you mean it.

{{< callout type="warning" >}}
`--dry-run` is the first thing to reach for on a command you have not run
before. It prints what would be changed and changes nothing. Lookups still run
for real, such as asking Slurm for a group or whether a node is running a job,
so a dry run refuses what the real run would refuse.
{{< /callout >}}
