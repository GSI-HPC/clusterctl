---
title: Install
weight: 1
---

clusterctl is one static binary. It needs no interpreter, no virtual
environment and nothing installed on the nodes.

## With mise

[mise](https://mise.jdx.dev) installs from the release assets, picks the build
for your platform, and checks the download against the release checksums and
the build provenance attestation:

```console
$ mise use -g github:GSI-HPC/clusterctl
$ clusterctl version
```

mise leaves a release alone for its first 24 hours (its `minimum_release_age`
setting), so on the day of a release it reports "no versions found ... matching
date filter" or picks the previous one. Name the version to take it sooner:

```console
$ mise use -g github:GSI-HPC/clusterctl@1.4.0
```

Pin a version for a team by putting it in the project's `mise.toml`:

```toml
[tools]
"github:GSI-HPC/clusterctl" = "1.4.0"
```

`mise install` then gives everyone the same binary, and `mise lock` records the
digest so a later install is checked against it:

```console
$ mise lock
$ git add mise.toml mise.lock
```

{{< callout type="info" >}}
There is no `mise use clusterctl` shorthand. mise's registry only takes widely
used tools, so the backend has to be named: `github:GSI-HPC/clusterctl`.
{{< /callout >}}

## From a release

Download the archive under its own name, together with `checksums.txt`:

```console
$ base=https://github.com/GSI-HPC/clusterctl/releases/latest/download
$ curl -fsSL --remote-name-all "$base/clusterctl_linux_amd64.tar.gz" "$base/checksums.txt"
```

Verify it before installing, against the checksums and against its build
provenance attestation, which says which workflow, commit and tag produced it:

```console
$ sha256sum -c checksums.txt --ignore-missing
$ gh attestation verify clusterctl_linux_amd64.tar.gz --repo GSI-HPC/clusterctl
```

Then install it:

```console
$ tar xzf clusterctl_linux_amd64.tar.gz
$ install -m 0755 clusterctl ~/.local/bin/
$ clusterctl version
```

The checksum answers "are these the published bytes"; the attestation answers
"who published them, from what". A download is worth both.

Archives exist for `linux_amd64`, `linux_arm64`, `darwin_amd64` and
`darwin_arm64`.

## From source

```console
$ go install github.com/GSI-HPC/clusterctl/cmd/clusterctl@latest
```

Go 1.26 or newer. An older toolchain downloads the right one by itself.

`go install` of a release reports the version it built, without a revision:

```console
$ clusterctl version
v1.4.0 go1.26.8 linux/amd64
```

A build from a git checkout reports its revision rather than a version, even
on a tagged commit, because the version of a release lives only in its signed
tag:

```console
$ go build ./cmd/clusterctl && ./clusterctl version
devel (a1b2c3d4e5f6) built 2026-09-22T14:42:30Z go1.26.8 linux/amd64
```

## What has to be on the other end

clusterctl installs nothing on your hosts, but it does run programs there.

**On your workstation:** an OpenSSH client, `sshuttle` if the site uses
tunnels, and [sops](https://getsops.io) 3.10.0 or later if the site keeps
secrets in `Secret` documents. clusterctl runs `sops` to decrypt one, the way it
runs `ssh`, so the sops that edits your secrets is the one that reads them.
A command that uses no secret runs without it. `clusterctl doctor` says whether
the one in `PATH` will do; `workstation.sopsBinary` names another. Install it
from your distribution, or pin it next to clusterctl with mise:

```console
$ mise use -g sops
```

**On the infrastructure hosts**, depending on which roles a site configures:

| Role | Programs |
| --- | --- |
| The Slurm host | `sinfo`, `squeue`, `sacct`, `sacctmgr`, `scontrol`, `getent` |
| The management gateway | `ipmipower` or `ipmitool`, whichever `bmc.ipmi.backend` names, and `fping` |
| The DHCP server | nothing; its files are read |
| The PXE host | `git`, if boot configurations come from version control |
| The fabric host | `ibportstate`, `ibqueryerrors`, `ibaddr`, `iblinkinfo`, `perfquery` |

`clusterctl doctor --remote` checks all of it and says what is missing.

## Shell completion

```console
$ clusterctl completion bash > /etc/bash_completion.d/clusterctl
$ clusterctl completion zsh  > "${fpath[1]}/_clusterctl"
$ clusterctl completion fish > ~/.config/fish/completions/clusterctl.fish
```

Completion knows your configuration: `-n` offers the groups your site defines
and `--context` offers your contexts. It never contacts a host, so the groups
of a source that runs a command, such as `@slurm:main`, are not offered and
have to be typed.
