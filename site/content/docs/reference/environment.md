---
title: Environment
weight: 2
---

## Variables

| Variable | Effect |
| --- | --- |
| `CLUSTERCTL_CONFIG` | Replaces the configuration search path. A `PATH`-style list of files and directories, most general first. |
| `CLUSTERCTL_CONTEXT` | Selects the context, as `--context` does. |
| `CLUSTERCTL_NODES` | The node set commands act on when neither `-n` nor a node set argument is given. An empty `-n` is an error, never a fall-back to it. |
| `CLUSTERCTL_PROGRESS` | How progress is shown when `--progress` is not given: `auto`, `tty`, `counter`, `plain` or `none`. Empty is `auto`, the live tree on a terminal. `plain` suits a CI job's log. Unlike the flag it fails no command: a display where none can be drawn shows nothing, and a value it does not take shows nothing, with a line on standard error. See [Progress](../../guides/progress/). |
| `CLUSTERCTL_PROGRESS_LOG` | The file each command appends its progress events to, as JSON lines, when `--progress-log` is not given; created readable by you alone. Empty writes none. Unlike the flag it fails no command: a file that cannot be used is not written, with a line on standard error. See [the event log](../../guides/progress/#the-event-log). |
| `CLUSTERCTL_FANOUT` | `fanout.max`, how many hosts ssh works on at once. Unlike `--fanout`, it leaves the service processors and the names asked at once to their own settings, `bmc.redfish.maxConcurrent`, `bmc.ipmi.maxConcurrent` and `services.dns.maxConcurrent`. |
| `CLUSTERCTL_CONNECT_TIMEOUT` | `ssh.connectTimeout` |
| `CLUSTERCTL_COMMAND_TIMEOUT` | `fanout.commandTimeout` |
| `CLUSTERCTL_KNOWN_HOSTS` | `ssh.knownHostsFile` |
| `CLUSTERCTL_SSH_BINARY` | The ssh client to run |
| `CLUSTERCTL_SCP_BINARY` | The scp client to run |
| `CLUSTERCTL_SSHUTTLE_BINARY` | The sshuttle to run |
| `CLUSTERCTL_SOPS_BINARY` | The sops that decrypts Secret documents, `workstation.sopsBinary` |
| `CLUSTERCTL_BROWSER` | `workstation.browser` |

A password is never one of these. `BMC_PASSWORD` is read only because a
credential in the configuration says `fromEnv: BMC_PASSWORD`.

`NO_COLOR` and `XDG_*` are honoured in the usual way; clusterctl draws no
colour anyway. `LC_ALL`, `LC_CTYPE` and `LANG`, the first one set, say whether
the terminal shows UTF-8: in any other locale the live tree and the counter are
drawn in ASCII.

`TRACEPARENT` and `TRACESTATE` are the W3C trace context a CI system that
traces its jobs may set. A valid `TRACEPARENT` gives a command's progress
events its trace id, and the first line the command writes to the
[event log](../../guides/progress/#the-event-log) records the span it names,
its trace flags and `TRACESTATE`, as they were given; one that is not valid
is ignored. Nothing is sent to a tracing system, and both variables are
taken out of the environment of the programs clusterctl runs, so none of
them traces under a span that no tracing system holds. Under `clusterctl mcp`,
`CLUSTERCTL_PROGRESS`, `CLUSTERCTL_PROGRESS_LOG` and `TRACEPARENT` are not
read for the commands an agent runs: each tool call is a trace of its own.

## Where files live

| Path | Holds |
| --- | --- |
| `/etc/clusterctl`, `$XDG_CONFIG_HOME/clusterctl` | The configuration, searched in that order |
| `$XDG_STATE_HOME/clusterctl` | The generated `ssh_config-*` files, one per configuration, multiplexing sockets, service processor certificate pins, tunnel process id files |
| `$XDG_CACHE_HOME/clusterctl` | Fetched copies of remote files, resolved group listings |

`$XDG_CONFIG_HOME` defaults to `~/.config`, `$XDG_STATE_HOME` to
`~/.local/state`, `$XDG_CACHE_HOME` to `~/.cache`. A relative value is ignored.
Without `HOME` and without an absolute `XDG_STATE_HOME` and `XDG_CACHE_HOME`,
as under `env -i` or in a system unit without `User=`, clusterctl refuses to
run rather than keep its state in a directory other users share.

Both the state and cache directories are created with mode 0700, and nothing in
either is authoritative: deleting them costs one round trip. One that is there
already must be a real directory you own that nobody else can write; otherwise
clusterctl refuses to run and `clusterctl doctor` says why.

## What clusterctl runs

**On your workstation:** `ssh`, `scp`, and `sshuttle` where tunnels are used.

**On an infrastructure host**, depending on the roles a site configures:
`ipmipower` or `ipmitool`, `fping`, the Slurm clients and `getent`,
`ibportstate`, `ibqueryerrors`, `ibaddr`, `iblinkinfo` and `perfquery`, `git`,
`tcpdump`, and `xargs`, which runs `ipmitool` for several processors and
`ibportstate` for several ports at once.

**On your workstation or the name servers:** the `dns` commands send their
queries themselves, to `services.dns.server` or else to the name servers in
`/etc/resolv.conf`; `/etc/hosts` is not consulted.

**On the nodes:** whatever a command was asked to run, plus `ibstat`,
`mlxconfig` and `mst` for the adapter commands and the configuration management
client for `cinc run`.

`clusterctl doctor --remote` checks all of it.
