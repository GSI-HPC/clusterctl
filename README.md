<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# clusterctl

One binary to administer HPC clusters: reach the hosts of a site, select nodes
with ClusterShell node set syntax, run commands on them in parallel, drive
their service processors, reinstall them and administer Slurm.

```console
$ clusterctl node select '@idle&@rack:R02'
exe[0004-0009]

$ clusterctl exec -n '@idle' --dedup -- uname -r
exe[0001-0009] (9)
  5.14.0-570.el9.x86_64
exe0010 (1)
  4.18.0-553.el8.x86_64

$ clusterctl slurm node drain 'ticket 4711: failing DIMM' -n exe0007
About to drain 1 host: exe0007
  reason: ticket 4711: failing DIMM
Continue? [y/N] y
drained exe0007

$ clusterctl provision reinstall -n '@rack:R02' --dry-run
Would reinstall 10 hosts: exe[0001-0010]
  everything on these machines is lost
```

**Manual:** <https://gsi-hpc.github.io/clusterctl/> ·
**Design notes:** [`doc/`](doc/)

## What it does

| | |
| --- | --- |
| **Reach everything** | One host key file the team keeps in version control, one generated ssh configuration, and sshuttle profiles for the networks behind a gateway. |
| **Select and fan out** | ClusterShell node sets, groups from node attributes, tables or the workload manager, and bounded parallel execution that reports every node. |
| **Operate hardware** | Redfish with a pinned certificate, FreeIPMI and ipmitool run on a host that can reach the service network, rack power units, and the InfiniBand fabric. |
| **Reinstall nodes** | DHCP inspection, per-node PXE and GRUB boot paths, age encrypted secrets streamed to the node, and the configuration management client. |
| **Administer Slurm** | Nodes and their drain reasons, the queue and the accounting database, accounts, users and fair share. |
| **Serve several clusters** | Domains, naming, roles, networks and the inventory live in YAML, not in the code. |

## Install

With [mise](https://mise.jdx.dev), which pins a version per project and
verifies the download:

```console
$ mise use -g github:GSI-HPC/clusterctl
$ clusterctl version
```

Or take the binary for your platform from the
[releases](https://github.com/GSI-HPC/clusterctl/releases):

```console
$ curl -fsSL -o clusterctl.tar.gz \
    https://github.com/GSI-HPC/clusterctl/releases/latest/download/clusterctl_linux_amd64.tar.gz
$ tar xzf clusterctl.tar.gz
$ install -m 0755 clusterctl ~/.local/bin/
```

Or build it:

```console
$ go install github.com/GSI-HPC/clusterctl/cmd/clusterctl@latest
```

Go 1.26 or newer. The toolchain downloads itself if yours is older.

## Get started

```console
$ mkdir -p ~/.config/clusterctl
$ cp examples/site/*.yaml ~/.config/clusterctl/    # then edit them
$ clusterctl config validate
$ clusterctl doctor
$ clusterctl node list
```

`examples/site/` is a complete configuration to copy. The
[manual](https://gsi-hpc.github.io/clusterctl/) walks through it, and
[`doc/migration.md`](doc/migration.md) maps every command and setting of the
shell toolkit clusterctl replaces to its counterpart.

## Shell completion

```console
$ clusterctl completion bash > /etc/bash_completion.d/clusterctl
$ clusterctl completion zsh  > "${fpath[1]}/_clusterctl"
```

Node set completion offers the configured groups, and `--context` offers the
configured contexts.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Everything succeeded |
| 1 | At least one target failed |
| 2 | Usage or configuration error |
| 3 | A host could not be reached |
| 130 | Interrupted, or a confirmation declined |

## Using the node set engine

The node set implementation is offered as a library, because the Go ecosystem
had none:

```go
import "github.com/GSI-HPC/clusterctl/nodeset"

ns, err := nodeset.Parse("exe[0001-0010]!exe0003")
fmt.Println(ns)          // exe[0001-0002,0004-0010]
fmt.Println(ns.Len())    // 9
```

## Contributing

The repository carries a `mise.toml`, so the toolchain comes from
[mise](https://mise.jdx.dev) if you use it:

```console
$ mise install  # Go and golangci-lint, at the versions CI uses
```

```console
$ make test     # unit and command tests
$ make lint     # vet and formatting
$ make cover    # coverage
$ make build    # bin/clusterctl
```

The documentation site needs Hugo **extended**; see [`site/README.md`](site/README.md).

Commits are [Conventional Commits](https://www.conventionalcommits.org/).
Design notes and the decision records are in [`doc/`](doc/); a change that
takes a decision gets a record.

## Licence

LGPL-3.0-or-later. See [`COPYING`](COPYING) and
[`COPYING.LESSER`](COPYING.LESSER).
