---
title: Troubleshooting
weight: 10
---

## Start here

```console
$ clusterctl doctor
$ clusterctl doctor --remote
```

`--remote` contacts every configured host role and checks the programs the
commands need are installed. Each role is asked in one session, and the roles
side by side, at most four sessions at once on one host; a jump host counts
too, since every session behind it connects there as well. These checks only
read, so `--dry-run` makes them too.

## A value is not what the file says

```console
$ clusterctl config explain fanout.max
FIELD   VALUE
path    fanout.max
value   6
layer   context
source
```

Seven layers can set a value: defaults, site, cluster, workstation, context,
environment, flags. `config explain` names the one that won and the line it was
written on. `clusterctl config view --show-sources` lists everything.

## The configuration will not load

```console
$ clusterctl config validate
clusterctl: site.yaml is not valid:
  site.yaml:9:7: spec.hosts.login.forwardAgnet: unknown field "forwardAgnet"; did you mean "forwardAgent"?
  site.yaml:13:5: spec.ssh.connectTimeout: got number, want string
```

A duration is a string: `30s`, not `30`. A bare number would read as
nanoseconds, which is never what a configuration means.

A file mode is a string too: `"0600"`. YAML parsers disagree about whether
`0600` is six hundred or octal, and a mode that silently becomes 384 only shows
up on the node.

## A connection fails

```console
$ clusterctl login --dry-run install
ssh -F ~/.local/state/clusterctl/ssh_config-3f9c2a1b7d4e5f60 -A -- root@installer.hpc.example.org
```

Run that command by hand with `-v`. Because clusterctl drives the ordinary ssh
client with a generated configuration, what you see is what it does.

```console
$ cat ~/.local/state/clusterctl/ssh_config-3f9c2a1b7d4e5f60
```

Common causes:

| Symptom | Likely cause |
| --- | --- |
| `Host key verification failed` | The key is not in the file: `clusterctl hostkey refresh -n HOST` |
| Exit code 3 and a timeout | The network is not reachable: a tunnel may need starting |
| `no matching host key type` | An sshd too old for current defaults: `legacyAlgorithms: true` on that role |
| A jump host is not used | `proxyJump` is a property of the role, not a flag |
| `terminating, 1 bad configuration options` | A misspelt keyword in `options` or an included file; `ssh -G -F FILE HOST` names it |
| Your own `~/.ssh/config` is not used | The site's `ssh.include` replaces the default list; add `~/.ssh/config` to it |

## A node set resolves to the wrong thing

```console
$ clusterctl node select '@compute' --expand | head
$ clusterctl node groups
$ clusterctl node groups exe0007
```

A group source that cannot be asked is named on the error stream, and the
command exits non-zero after printing what the other sources answered. A bare
`@group` does not fall back to another source when one fails: the error names
the source that stopped the search.

Remember that operators are evaluated strictly left to right with no
precedence, so `@a!@b&@c` is `((@a minus @b) intersect @c)`.

If a name comes back with different padding than you typed, that is expected:
`exe1` and `exe0001` are the same host, and a selection is reported under the
name the inventory gave it.

## A service processor is refused

```console
clusterctl: the certificate of exe0001.mgmt.hpc.example.org changed:
  recorded sha256:1a2b…, now sha256:9f8e…
```

The certificate is pinned on first sight. If it was replaced on purpose:

```console
$ clusterctl bmc forget exe0001
```

If it was not, find out why before clearing it.

## Redfish rejects an action

```console
clusterctl: exe0001.mgmt…: does not accept the reset type "GracefulShutdown";
  it accepts ForceOff, ForceRestart, GracefulRestart, On, PowerCycle
```

That firmware does not implement it. Either use one it accepts, or record the
list for that hardware:

```yaml
bmc:
  vendors:
    vendor2:
      resetTypes: [On, ForceOff, ForceRestart, GracefulRestart, PowerCycle]
```

A recorded list replaces what the machine advertises, and the message then
says `its vendor profile lists` instead of `it accepts`.

## A service processor answers with a redirect

```console
clusterctl: exe0001.mgmt…: /redfish/v1/Systems/1 answered with a redirect to
  "http://exe0001.mgmt…:8080/redfish/v1/Systems/1", which is not followed
```

Redirects are refused: following one could send an action twice, or the
credentials over plain HTTP. Point `bmc.redfish.systemPath` or the vendor's
`systemPath` at the resource the service processor serves, or fix its
configuration.

## A command times out on the node

The timeout is enforced on the node with `timeout`, so the remote process is
stopped rather than left running:

```console
$ clusterctl exec -n '@compute' --timeout 5m -- ./slow-check
```

## Everything is slow

Watch where the time goes first. On a terminal the live tree names the step a
command is in and, under it, the targets that have run longest, with the
request each one waits for; a lookup that takes more than a second shows up
too. `--progress plain` says the same in lines, which a CI log keeps, and
`--progress-log` keeps when every step, target and request started and ended,
and how:

```console
$ clusterctl --progress plain bmc status -n '@compute'
$ clusterctl --progress-log slow.jsonl bmc status -n '@compute'
$ jq -c 'select(.type == "end" and .class == "timeout") | [.name, .host, .err]' slow.jsonl
```

See [Progress](../progress/). How many hosts are worked on at once depends
on the kind of work, and each kind has a bound of its own:

| Work | Bound | Default |
| --- | --- | --- |
| ssh and scp to nodes, host key scans | `fanout.max`, or `--fanout` | 16 |
| Redfish requests | `bmc.redfish.maxConcurrent` | 8 |
| ipmitool, on the host `bmc.ipmi.via` names | `bmc.ipmi.maxConcurrent` | 8 |
| The names of `dns lookup` | `services.dns.maxConcurrent` | 16 |

```console
$ clusterctl exec --fanout 32 -n '@compute' -- uptime
$ clusterctl --set bmc.redfish.maxConcurrent=16 bmc status -n '@compute'
```

`--fanout` sets the first, and lowers the others where it is lower, but never
raises them; raise one of those in the site's configuration, or with `--set`
for one command. The default fan-out is conservative because connections
through a tunnel or a jump host are fragile and a wide fan-out trips sshd's
`MaxStartups`. Raise it when the path is direct. A service processor is far
slower than a node and has few connections to give, and a site's resolver
may limit how fast it is asked, so raise theirs with care. A power-on or a
power cycle is slow on purpose: it is sent in batches of
`safety.powerOnBatch`, with `safety.powerOnStagger` between them, and
`--batch` and `--stagger` change both for one command.

## Something changed that should not have

Every destructive command understands `--dry-run` and prints what it would do
without changing anything. Lookups, such as resolving a group, asking Slurm
about a node or checking a boot path on the PXE host, still run. It is also what to use when reading an unfamiliar
command's behaviour:

```console
$ clusterctl provision reinstall -n '@rack:R02' --dry-run
$ clusterctl boot set -n exe0007 --dry-run
```
