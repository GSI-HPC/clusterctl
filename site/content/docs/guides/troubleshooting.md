---
title: Troubleshooting
weight: 8
---

## Start here

```console
$ clusterctl doctor
$ clusterctl doctor --remote
```

`--remote` contacts every configured host role and checks the programs the
commands need are installed.

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
ssh -F ~/.local/state/clusterctl/ssh_config -A root@installer.hpc.example.org
```

Run that command by hand with `-v`. Because clusterctl drives the ordinary ssh
client with a generated configuration, what you see is what it does.

```console
$ cat ~/.local/state/clusterctl/ssh_config
```

Common causes:

| Symptom | Likely cause |
| --- | --- |
| `Host key verification failed` | The key is not in the file: `clusterctl hostkey refresh -n HOST` |
| Exit code 3 and a timeout | The network is not reachable: a tunnel may need starting |
| `no matching host key type` | An sshd too old for current defaults: `legacyAlgorithms: true` on that role |
| A jump host is not used | `proxyJump` is a property of the role, not a flag |

## A node set resolves to the wrong thing

```console
$ clusterctl node select '@compute' --expand | head
$ clusterctl node groups
$ clusterctl node groups exe0007
```

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

## A command times out on the node

The timeout is enforced on the node with `timeout`, so the remote process is
stopped rather than left running:

```console
$ clusterctl exec -n '@compute' --timeout 5m -- ./slow-check
```

## Everything is slow

```console
$ clusterctl exec --fanout 32 -n '@compute' -- uptime
```

The default fan-out is conservative because connections through a tunnel or a
jump host are fragile and a wide fan-out trips sshd's `MaxStartups`. Raise it
when the path is direct.

## Something changed that should not have

Every destructive command understands `--dry-run` and prints what it would do
without sending anything. It is also what to use when reading an unfamiliar
command's behaviour:

```console
$ clusterctl provision reinstall -n '@rack:R02' --dry-run
$ clusterctl boot set -n exe0007 --dry-run
```
