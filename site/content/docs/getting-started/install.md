---
title: Install
weight: 1
---

clusterctl is one static binary. It needs no interpreter, no virtual
environment and nothing installed on the nodes.

## From a release

```console
$ curl -fsSL -o clusterctl.tar.gz \
    https://github.com/GSI-HPC/clusterctl/releases/latest/download/clusterctl_linux_amd64.tar.gz
$ tar xzf clusterctl.tar.gz
$ install -m 0755 clusterctl ~/.local/bin/
$ clusterctl version
```

Releases carry `checksums.txt`. Verify before installing:

```console
$ sha256sum -c checksums.txt --ignore-missing
```

Archives exist for `linux_amd64`, `linux_arm64`, `darwin_amd64` and
`darwin_arm64`.

## From source

```console
$ go install github.com/GSI-HPC/clusterctl/cmd/clusterctl@latest
```

Go 1.26 or newer. An older toolchain downloads the right one by itself.

A build from source reports its revision rather than a version, because the
version of a release lives only in its signed tag:

```console
$ clusterctl version
devel (a1b2c3d4e5f6) built 2026-09-22T14:42:30Z go1.26.0 linux/amd64
```

## What has to be on the other end

clusterctl installs nothing on your hosts, but it does run programs there.

**On your workstation:** an OpenSSH client, and `sshuttle` if the site uses
tunnels.

**On the infrastructure hosts**, depending on which roles a site configures:

| Role | Programs |
| --- | --- |
| The Slurm host | `sinfo`, `squeue`, `sacct`, `sacctmgr`, `scontrol`, `getent` |
| The management gateway | `ipmipower` or `ipmitool`, `fping` |
| The DHCP server | nothing; its files are read |
| The PXE host | `git`, if boot configurations come from version control |
| The fabric host | `ibportstate`, `ibqueryerrors` |

`clusterctl doctor --remote` checks all of it and says what is missing.

## Shell completion

```console
$ clusterctl completion bash > /etc/bash_completion.d/clusterctl
$ clusterctl completion zsh  > "${fpath[1]}/_clusterctl"
$ clusterctl completion fish > ~/.config/fish/completions/clusterctl.fish
```

Completion knows your configuration: `-n` offers the groups your site defines
and `--context` offers your contexts.
