---
title: Configuration
weight: 1
---

## Documents

Each document carries an `apiVersion` of `clusterctl/v1alpha1` and a `kind`.
Several may share a file, separated by `---`.

| Kind | Scope |
| --- | --- |
| `Config` | Per administrator: contexts and which is current |
| `Site` | One site |
| `Cluster` | One cluster in a site |
| `NodeInventory` | The nodes |
| `Workstation` | The machine clusterctl runs on |

The authoritative field list is the JSON Schema:

```console
$ clusterctl config schema Site
```

or `https://gsi-hpc.github.io/clusterctl/schema/v1alpha1/site.json`.

## Merge layers

Applied in this order, each winning over the ones before it:

1. built-in defaults
2. `Site`
3. `Cluster`, then its `overrides`
4. `Workstation`, then its `overrides`
5. the current context's `overrides`
6. the environment
7. `--set` and the command line

Mappings merge key by key. Sequences and scalars replace: a list of naming
rules only makes sense as a whole.

Each layer is validated against the schema of its kind **before** it is merged,
so a mistake is reported at the line it was written on.

## Config

```yaml
apiVersion: clusterctl/v1alpha1
kind: Config
currentContext: cluster1
contexts:
  - name: cluster1
    cluster: cluster1        # the Cluster document to act on
    user: alice_adm          # the default remote account
    overrides:               # dotted paths into the merged configuration
      fanout.max: 6
```

## Site

| Field | What it does |
| --- | --- |
| `domains` | DNS domains by role: `hpc`, `site`, `mgmt`, `mgmtHpc`, `infra` |
| `naming.rules` | Short name to host name and service processor name; first match wins |
| `naming.bmcPrefix` | Available to templates as `{bmcPrefix}` |
| `hosts` | Infrastructure roles: `host`, `user`, `forwardAgent`, `forwardX11`, `proxyJump`, `controlMaster`, `legacyAlgorithms`, `options`, `description` |
| `networks` | Named CIDRs, used by tunnels |
| `credentials` | Named accounts and where their password is read from |
| `secrets.recipients` | age or OpenSSH public keys that `secrets encrypt` encrypts to |
| `bmc` | Out-of-band access: `credential`, `order`, `ipmi`, `redfish`, `pdu`, `vendors` |
| `ssh` | Transport: `knownHostsFile`, `include`, timeouts, `sendEnv`, `options`, `binary` |
| `tunnels` | sshuttle profiles: `remote`, `subnets`, `excludes`, `dns`, `method` |
| `safety` | `protectedHosts`, `confirmAbove`, `slurmAware`, `powerOnBatch`, `powerOnStagger` |
| `fanout` | `max`, `connectTimeout`, `commandTimeout`, `offloadAbove`, `offload` |
| `services` | `dhcp`, `pxesrv`, `tftp`, `http`, `cinc`, `mail`, `fabric`, `dns` |

### Naming templates

`{name}`, `{bmcPrefix}` and `{domains.X}` for any configured domain. A template
referring to an unset domain is reported rather than producing a name that ends
in a dot.

### Password sources

Exactly one per credential:

| Source | Reads |
| --- | --- |
| `fromEnv` | An environment variable |
| `file` | The first line of a file |
| `ageFile` | An age encrypted file, with `workstation.identities` |
| `age` | age ciphertext written inline, with `workstation.identities` |
| `command` | The standard output of a helper |
| `prompt` | The terminal |

## Cluster

```yaml
apiVersion: clusterctl/v1alpha1
kind: Cluster
metadata:
  name: cluster1
spec:
  site: example
  slurm:
    role: login
    organization: example
    defaultAccount: default
    partitions: [main, debug]
    lookBack: 1h
  groups:
    defaultSource: inventory
    sources: {}
  bootPaths:
    - nodes: exe[0001-1024]
      path: /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2
  inventories: []       # empty means every loaded NodeInventory
  overrides: {}
```

### Group sources

| Field | Meaning |
| --- | --- |
| `static` | A table: group name to node set expression |
| `attribute` | One group per value of a node attribute |
| `exec` | Commands run on a host role: `map`, `all`, `list`, `reverse` |
| `cacheTtl` | How long a resolved group is reused |

`$GROUP` and `$NODE` in an `exec` vector are substituted as whole arguments.

## NodeInventory

```yaml
apiVersion: clusterctl/v1alpha1
kind: NodeInventory
spec:
  defaults:
    attributes: {os: el9}
  nodes:
    - nodes: exe[0001-1024]          # a node set expression
      attributes: {class: exe, vendor: vendor2}
      rack: R02
    - nodes: exe0001                 # a later entry refines an earlier one
      address: 10.0.2.1
      cid: "223456789"
      macs: ["00:11:22:33:44:55"]
      level: "1"
      bootPath: /srv/pxesrv/boot/special
```

`rack` and `level` are also exposed as attributes, so a group source reading an
attribute can build one group per rack.

Fields describing a single machine — `address`, `bmcAddress`, `cid`, `macs` —
may only be set by an entry naming exactly one node.

## Workstation

```yaml
apiVersion: clusterctl/v1alpha1
kind: Workstation
spec:
  host: desk01.example.org     # {workstation.host} in tunnel templates
  addresses: {}
  identities: [~/.ssh/id_ed25519]
  browser: firefox
  pager: less
  sshuttleBinary: sshuttle
  overrides: {}
```

Name the document after the host to keep several machines in one file.

## Paths

A path resolves against the directory of the `Site` document. A leading `~` is
expanded. Absolute paths are left alone.

## Types

**Durations** are strings: `30s`, `5m`, `1h30m`. A bare number is rejected
because it would read as nanoseconds.

**File modes** are strings: `"0600"`. A number with a leading zero is read as a
string for the same reason.
