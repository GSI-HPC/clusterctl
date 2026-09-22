---
title: Environment
weight: 2
---

## Variables

| Variable | Effect |
| --- | --- |
| `CLUSTERCTL_CONFIG` | Replaces the configuration search path. A `PATH`-style list of files and directories, most general first. |
| `CLUSTERCTL_CONTEXT` | Selects the context, as `--context` does. |
| `CLUSTERCTL_NODES` | The node set commands act on when `-n` is not given. |
| `CLUSTERCTL_FANOUT` | `fanout.max` |
| `CLUSTERCTL_CONNECT_TIMEOUT` | `ssh.connectTimeout` |
| `CLUSTERCTL_COMMAND_TIMEOUT` | `fanout.commandTimeout` |
| `CLUSTERCTL_KNOWN_HOSTS` | `ssh.knownHostsFile` |
| `CLUSTERCTL_SSH_BINARY` | The ssh client to run |
| `CLUSTERCTL_SCP_BINARY` | The scp client to run |
| `CLUSTERCTL_SSHUTTLE_BINARY` | The sshuttle to run |
| `CLUSTERCTL_PAGER` | `workstation.pager` |
| `CLUSTERCTL_BROWSER` | `workstation.browser` |

A password is never one of these. `BMC_PASSWORD` is read only because a
credential in the configuration says `fromEnv: BMC_PASSWORD`.

`NO_COLOR` and `XDG_*` are honoured in the usual way.

## Where files live

| Path | Holds |
| --- | --- |
| `/etc/clusterctl`, `$XDG_CONFIG_HOME/clusterctl` | The configuration, searched in that order |
| `$XDG_STATE_HOME/clusterctl` | The generated `ssh_config`, multiplexing sockets, service processor certificate pins, tunnel process id files |
| `$XDG_CACHE_HOME/clusterctl` | Fetched copies of remote files, resolved group listings |

`$XDG_CONFIG_HOME` defaults to `~/.config`, `$XDG_STATE_HOME` to
`~/.local/state`, `$XDG_CACHE_HOME` to `~/.cache`.

Both the state and cache directories are created with mode 0700, and nothing in
either is authoritative: deleting them costs one round trip.

## What clusterctl runs

**On your workstation:** `ssh`, `scp`, and `sshuttle` where tunnels are used.

**On an infrastructure host**, depending on the roles a site configures:
`ipmipower` or `ipmitool`, `fping`, the Slurm clients and `getent`,
`ibportstate` and `ibqueryerrors`, `git`, `tcpdump`.

**On the nodes:** whatever a command was asked to run, plus `ibstat`,
`mlxconfig` and `mst` for the adapter commands and the configuration management
client for `cinc run`.

`clusterctl doctor --remote` checks all of it.
