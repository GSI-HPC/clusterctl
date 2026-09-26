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

`login` takes one name. A command goes after `--`; a second word before it is
refused rather than dropped, so `clusterctl login mgmt uptime` is an error and
`clusterctl login mgmt -- uptime` runs `uptime`.

## On many hosts

```console
$ clusterctl exec -n '@compute' -- uptime -p
$ clusterctl exec 'exe[1-4]' -r -- systemctl is-active slurmd
$ clusterctl exec -n '@compute' --dedup -- uname -r
```

The command always follows `--`. Everything before it belongs to clusterctl,
so without it the `-r` of `shutdown -r now` would be read as `--root` and the
`-n` of `grep -n` would replace the node set. exec refuses a command that is
not preceded by `--` rather than guess:

```console
$ clusterctl exec -n exe0001 shutdown -r now
clusterctl: the command has to follow --, so that its options are not read as clusterctl's: clusterctl exec [-n NODESET] -- COMMAND...
```

The node set goes before `--`, as for every other node command, or in `-n`. It
wins over `CLUSTERCTL_NODES`. Giving it both ways is refused, because a word
of the command written before `--` would otherwise be taken for a node:

```console
$ clusterctl exec -n exe0001 sudo -- reboot
clusterctl: "sudo" is a node set, and -n already names one; give the nodes once, and the command after --
```

`--dedup` collapses the nodes that answered the same thing, and ended the same
way, which is what makes a thousand-node answer readable. Each group says how
its nodes ended: `ok`, `exit N`, `unreachable` or `interrupted`, so that for
`grep -q` or `test -e`, where the status is the whole answer, the groups still
tell the nodes apart:

```console
$ clusterctl exec -n '@compute' --dedup -- rpm -q slurm
exe[0001-1020] (1020): ok
  slurm-24.05.4-1.el9.x86_64
exe[1021-1024] (4): ok
  slurm-23.11.6-1.el9.x86_64
```

## Protected hosts and confirmation

exec does not ask before it runs, but a host in `safety.protectedHosts` is
refused on every run, and only `--force` gets past. `--confirm` adds the
question the destructive commands ask. `--dry-run` shows the same preview and
makes the same decision as the real run.

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

The input is read once and replayed to each node. If it cannot be read to the
end, for example because a directory was redirected, nothing is sent and exec
exits 2: a truncated file on every node is worse than none.

Standard input carries the payload, so the question `--confirm` asks cannot be
read from it. `--stdin --confirm` needs `-y`, and `--dry-run` previews it as
usual, with the size of the payload.

## Timeouts

A command timeout is enforced **on the node** with `timeout`, because killing
the local ssh would leave the remote process running:

```console
$ clusterctl exec -n '@compute' --timeout 30s -- ./slow-check
```

Without a flag, `fanout.commandTimeout` from the configuration applies.

A node that stops answering cannot end its command, so clusterctl stops ssh
itself when the command has not ended five seconds after its timeout plus the
time reaching the node may take, and reports the node as unreachable, exit
code 3. Reaching a node may take `ssh.connectionAttempts` attempts of
`ssh.connectTimeout`, a second apart, 21 seconds with the defaults, and as
long again for each jump host a role's `proxyJump` puts in front of it.

## How wide it goes

```console
$ clusterctl exec -n '@compute' --fanout 4 -- uptime
```

The default is conservative, because a connection through a tunnel or a jump
host is more fragile than a local one, and because a wide fan-out is what makes
sshd refuse connections under `MaxStartups`.

Redfish requests go to `bmc.redfish.maxConcurrent` service processors at a
time, 8 by default, whatever `fanout.max` says: a processor is much slower
than a node, and a site that widens the fan-out for its nodes does not mean to
widen it for its processors. A `--fanout` below that lowers it too, so
`--fanout 1` asks one processor at a time; one above it does not raise it, so
`--fanout 64` still asks 8 at once.

ipmitool, which takes one processor at a time, is run on the host
`bmc.ipmi.via` names for `bmc.ipmi.maxConcurrent` processors at once, 8 by
default, in one ssh session. `dns lookup` resolves
`services.dns.maxConcurrent` names at a time, 16 by default, since a site's
resolver may limit how fast it is asked. The same rule applies to both: a
lower `--fanout` lowers them, a higher one does not raise them, and
`fanout.max` does not change them. ipmipower is handed the whole set and
fans out by itself, to FreeIPMI's own limit, or to `--fanout` when it is
given, higher or lower.

`fanout.max` and `--fanout` have to be at least 1. A `0` or a negative value
is refused, by `config validate` with the file and line it was written on,
rather than quietly read as the default.

## Copying files

```console
$ clusterctl copy -n '@compute' /etc/hosts /etc/hosts
$ clusterctl copy -n '@compute' -R ./config /etc/myapp/
$ clusterctl copy -n 'exe[1-4]' --download /var/log/slurmd.log /var/log/messages ./logs/
```

Downloading from several nodes needs a directory, ending in a slash: each
node's files land in a directory of its own under it, `./logs/exe0001/` and so
on, because otherwise every node would write to the same path.

The nodes are copied in parallel, as many at once as `fanout.max` or
`--fanout` allow. Each transfer is bounded by `--timeout`, which defaults to
`fanout.commandTimeout` and, when that is unset too, to 30 minutes; a transfer
that runs over is reported as failed and the other nodes carry on.

scp draws its progress meter on standard error only when one transfer runs at
a time, for one node or with `--fanout 1`, and no progress is shown. The meter
redraws one line of the terminal, so the meters of transfers running side by
side would overwrite each other, the counter's line, and plain lines; none is
drawn then. `--progress none` leaves the line to scp.

A remote path must not hold whitespace, quotes or shell syntax such as `$`,
`;` or `(`. The legacy scp protocol, which OpenSSH used by default before 9.0
and RHEL 8 still does, hands the path to the remote shell, while SFTP takes it
literally, so such a path is refused rather than read one of two ways. Globs
and a leading `~` mean the same to both and work.

## Reading the result

A failing node does not stop the others, and every node is reported:

```console
$ clusterctl exec -n 'exe[1-4]' -- systemctl is-active slurmd
exe0001: active
exe0002: active
exe0003: failed
exe0004: active
exe0003: exit 3
clusterctl: 1 of 4 hosts failed: exe0003
$ echo $?
1
```

Each node that failed gets a line on standard error with how it ended and the
last line it wrote to standard error, for example
`exe0005: exit 5: Unit slurmd.service could not be found.` or
`exe0006: unreachable: ssh: connect to host exe0006 port 22: Connection refused`.

Exit code 1 means clusterctl worked and a target failed. Exit code 3 means a
host could not be reached at all, 2 that the configuration lacks something a
node needs, and 130 that the run was interrupted before every node had
answered. When several of these happen in one run, the exit code is the first
of 130, 3, 2 and 1 that applies.

What a node prints is its own, so control characters in it are shown as escapes
such as `\x1b`, `\r` or `\u202e` rather than passed to your terminal, where they
could overwrite another node's line or show it in another order. A tab is kept.
The machine formats carry the output unchanged.

For a machine, ask for it:

```console
$ clusterctl exec -n '@compute' -o json -- uptime -p | jq -r '.[] | select(.exitCode != 0) | .target.name'
```
