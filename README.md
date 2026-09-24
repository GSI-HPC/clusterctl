<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# clusterctl

**One binary. Every node. No surprises.** clusterctl selects nodes with
ClusterShell syntax, fans out commands, drives service processors, reinstalls
nodes and administers Slurm, and asks before it changes anything.

### [Read the manual →](https://gsi-hpc.github.io/clusterctl/)

```console
$ clusterctl node select '@slurm:main&@rack:R02'
exe[0004-0009]

$ clusterctl exec -n '@slurm:main' --dedup -- uname -r
exe[0001-0009] (9): ok
  5.14.0-570.el9.x86_64
exe0010 (1): ok
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
$ clusterctl config init --site lab --cluster alpha --domain hpc.example.org
$ $EDITOR ~/.config/clusterctl/*.yaml    # fill in what the comments ask for
$ clusterctl config validate
$ clusterctl doctor
$ clusterctl node list
```

`config init` writes the least configuration that resolves, and only into an
empty directory. `examples/site/` is a complete
configuration to take further settings from. The
[manual](https://gsi-hpc.github.io/clusterctl/) walks through both, and
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

Copyright (C) 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH
<http://www.gsi.de>

LGPL-3.0-or-later. See [`COPYING`](COPYING) and
[`COPYING.LESSER`](COPYING.LESSER).
