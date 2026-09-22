---
title: Running commands
weight: 2
---

## On one host

```console
$ clusterctl login                       # the login role
$ clusterctl login mgmt                  # a role by name
$ clusterctl login exe0007               # a node
$ clusterctl login -r install            # as root
$ clusterctl login install -- ls /srv/pxesrv/boot
```

A name that matches a configured role connects with that role's account and
options. Anything else is treated as a host or a node and resolved through the
naming rules.

## On many hosts

```console
$ clusterctl exec -n '@compute' -- uptime -p
$ clusterctl exec -n 'exe[1-4]' -r -- systemctl is-active slurmd
$ clusterctl exec -n '@compute' --dedup -- uname -r
```

`--dedup` collapses the nodes that answered the same thing, which is what makes
a thousand-node answer readable:

```console
$ clusterctl exec -n '@compute' --dedup -- rpm -q slurm
exe[0001-1020] (1020)
  slurm-24.05.4-1.el9.x86_64
exe[1021-1024] (4)
  slurm-23.11.6-1.el9.x86_64
```

## Your command arrives intact

The argument vector is quoted once and reassembled by the remote shell, so it
arrives exactly as you typed it:

```console
$ clusterctl exec -n exe0001 -- ls '/var/log/*.log'
$ clusterctl exec -n exe0001 -- grep "it's here" /etc/motd
$ clusterctl exec -n exe0001 -- echo 'a  b'
```

The glob is expanded on the node, not on your laptop. The apostrophe survives.
Repeated whitespace survives.

## Scripts

For anything with a pipeline or a conditional, use `--script`:

```console
$ clusterctl exec -n '@compute' --script '
    if systemctl is-active --quiet slurmd; then
      echo up
    else
      journalctl -u slurmd -n 3 --no-pager
    fi'
```

A script is sent as one argument to the remote shell, so standard input stays
free for data.

## Sending data to every node

```console
$ clusterctl exec -n '@compute' --stdin --script 'cat > /etc/motd' < motd.txt
```

The input is read once and replayed to each node.

## Timeouts

A command timeout is enforced **on the node** with `timeout`, because killing
the local ssh would leave the remote process running:

```console
$ clusterctl exec -n '@compute' --timeout 30s -- ./slow-check
```

Without a flag, `fanout.commandTimeout` from the configuration applies.

## How wide it goes

```console
$ clusterctl exec -n '@compute' --fanout 4 -- uptime
```

The default is conservative, because a connection through a tunnel or a jump
host is more fragile than a local one, and because a wide fan-out is what makes
sshd refuse connections under `MaxStartups`.

## Copying files

```console
$ clusterctl copy -n '@compute' /etc/hosts /etc/hosts
$ clusterctl copy -n '@compute' -R ./config /etc/myapp/
$ clusterctl copy -n 'exe[1-4]' --download /var/log/slurmd.log ./logs/
```

Downloading from several nodes needs a directory, ending in a slash: each file
lands in it under the node's name, because otherwise every node would write to
the same path.

## Reading the result

A failing node does not stop the others, and every node is reported:

```console
$ clusterctl exec -n 'exe[1-4]' -- systemctl is-active slurmd
exe0001: active
exe0002: active
exe0004: active
exe0003: failed
clusterctl: 1 of 4 hosts failed: exe0003
$ echo $?
1
```

Exit code 1 means clusterctl worked and a target failed. Exit code 3 would mean
a host could not be reached at all.

For a machine, ask for it:

```console
$ clusterctl exec -n '@compute' -o json -- uptime -p | jq -r '.[] | select(.exitCode != 0) | .target.name'
```
