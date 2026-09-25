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
| `Secret` | Values encrypted with sops, alone in their file |

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

A `Site`, `Cluster`, `NodeInventory`, `Workstation` or `Secret` is defined
once: a second document of the same kind and name, such as a stale
`site_old.yaml`, is an error that names both. Only a context of the `Config`
document may be defined again, in a later file, and then replaces the earlier
one.

## Overrides

An `overrides` table, of a `Cluster`, a `Workstation` or a context, and
`--set PATH=VALUE` address the merged configuration by dotted path. Each path
and value is checked against the schema of the merged configuration, in every
context and not only the current one: a key is matched with the case it was
written in, so `safety.protectedhosts` is refused with a suggestion rather than
taken for `safety.protectedHosts`, and a value of the wrong type, `null`
included, is refused. A mapping value merges key by key, the way a mapping in
a document does, so these two mean the same and neither touches
`safety.protectedHosts`:

```yaml
overrides:
  safety.confirmAbove: 4
  safety: {confirmAbove: 4}
```

A relative path given in the environment or with `--set` is resolved against
the working directory; one written in a document is resolved against the
directory of the `Site` document.

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
| `bmc` | Out-of-band access: `credential`, `order`, `ipmi`, `redfish`, `pdu`, `vendors` |
| `ssh` | Transport: `knownHostsFile`, `include`, timeouts, `sendEnv`, `options`, `binary` |
| `tunnels` | sshuttle profiles: `remote`, `subnets`, `excludes`, `dns`, `method` |
| `safety` | `protectedHosts`, `confirmAbove`, `slurmAware`, `powerOnBatch`, `powerOnStagger` |
| `fanout` | `max`, `connectTimeout`, `commandTimeout` |
| `services` | `dhcp`, `pxesrv`, `tftp`, `http`, `cinc`, `mail`, `fabric`, `dns` |

### Naming templates

`{name}`, `{bmcPrefix}` and `{domains.X}` for any configured domain. A template
referring to an unset domain is reported rather than producing a name that ends
in a dot.

A `pattern` must match the whole short name, and names are lowercased before
any rule is tried. A node's service processor is named only by the `bmc`
template of the first rule that matches it. A node whose rule has none, that
no rule matches, or whose template gives the node's own short name or host name
back, has no service processor name: the `bmc` commands refuse it unless its
inventory entry sets `bmcAddress`.

### Password sources

Exactly one per credential:

| Source | Reads |
| --- | --- |
| `fromEnv` | An environment variable |
| `file` | The first line of a file |
| `ageFile` | An age encrypted file, with `workstation.identities` |
| `secretRef` | `{name, key}` of a `Secret` document |
| `command` | The first line a helper prints; a helper named by a relative path resolves against the `Site` document, a bare name is looked up in `PATH` |
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
    lookBack: 1h
  groups:
    defaultSource: inventory
    sources: {}
  bootPaths:
    - nodes: exe[0001-1024]
      path: /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2
  inventories: []       # empty: every NodeInventory, refused when several sites are loaded
  overrides: {}
```

### Group sources

| Field | Meaning |
| --- | --- |
| `static` | A table: group name to node set expression |
| `attribute` | One group per value of a node attribute |
| `exec` | Commands run on a host role: `role` (required), `map`, `all`, `list`, `reverse` |
| `cacheTtl` | How long an `exec` answer is reused: a resolved group, `all` and `list` |

`$GROUP` and `$NODE` in an `exec` vector are substituted as whole arguments.
Each command is bounded by `fanout.commandTimeout`. A cached answer is keyed by
the site, cluster and context, the host and the exact command, so it is never
reused for another cluster or another group name.

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

Entries are applied in order, and a later entry refines the nodes it names.
Write a host the same way in every entry: `exe1` and `exe0001` are one host,
so an entry naming `exe1` after one naming `exe[0001-1024]` is refused rather
than guessed at. The error names both entries and the spelling to use. Write
names in lower case: host names are not case sensitive, so `EXE0001` is
refused with the spelling `exe0001`.

`rack` and `level` are also exposed as attributes, so a group source reading an
attribute can build one group per rack. Writing either the field or the
attribute sets both, so `attributes: {rack: R05}` moves a node to rack `R05`
for `node rack` and `@rack:R05` alike. An entry writing both with different
values is refused.

Fields describing a single machine — `address`, `bmcAddress`, `cid`, `macs` —
may only be set by an entry naming exactly one node, and no two nodes may share
a value: a copied address would send a reinstall meant for one machine to
another. An address and a `bmcAddress` share one space, and MACs are compared
however they are written. `address` must be an IP address, without a prefix
length or zone; `bmcAddress` an IP address or a host name; each of `macs` a
48-bit MAC address. Whether these hold is judged on what the inventory ends up
holding, so a refinement that moves an address away frees it. `config validate`
reports a violation with the file and line of each entry involved.

`bmcAddress` wins over the name the naming rules derive: the `bmc` commands,
`provision` and the recorded certificate all use it.

## Workstation

```yaml
apiVersion: clusterctl/v1alpha1
kind: Workstation
spec:
  host: desk01.example.org     # {workstation.host} in tunnel templates
  addresses: {}
  identities: [~/.ssh/id_ed25519]
  sopsKeyTypes: [age]          # the kinds of key a Secret may be encrypted to
  browser: firefox
  pager: less
  sshuttleBinary: sshuttle
  overrides: {}
```

Name the document after the host to keep several machines in one file.

## Secret

```yaml
apiVersion: clusterctl/v1alpha1
kind: Secret
metadata:
  name: example                  # what secretRef.name refers to
data:
  bmc-password: ENC[AES256_GCM,...]   # text; quote 0600, true or a date
binaryData:
  munge-key: ENC[AES256_GCM,...]      # base64, decoded before use
sops: {}                         # written by sops
```

Encrypt only the values, so that the kind, the name and the keys stay readable:

```console
$ sops --encrypt --encrypted-regex '^(data|binaryData)$' --in-place secrets.sops.yaml
```

A reference, `secretRef: {name: example, key: bmc-password}`, is a password
source in a credential and replaces `source` in `services.cinc.secrets`. It is
checked against the keys when the configuration loads and decrypted when a
command uses it: with `workstation.identities` first, then, at a terminal
only, with the keys sops finds itself (`SOPS_AGE_KEY_FILE`, a PGP agent,
cloud KMS or Vault credentials), age and PGP before any service.

A Secret encrypted to a kind of key that `workstation.sopsKeyTypes` does not
list, age alone when unset, is refused before any key is tried: the sops
metadata that names the keys is not authenticated, and a Vault address added
there would otherwise receive your Vault token. A value sops did not store as
text, such as an unquoted `0600`, and a file encrypted with
`--mac-only-encrypted` are refused when the configuration loads.

Hidden files in a configuration directory are not read, so `.sops.yaml` can sit
next to the documents.

## Paths

A path resolves against the directory of the `Site` document. A leading `~` is
expanded. Absolute paths are left alone.

## Types

**Durations** are strings: `30s`, `5m`, `1h30m`. A bare number is rejected
because it would read as nanoseconds.

**File modes** are strings: `"0600"`. A number with a leading zero is read as a
string for the same reason.
