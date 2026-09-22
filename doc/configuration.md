<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Configuration

The shell toolkit carried about ninety flat environment variables, data in five
formats, and no way to tell which of them a value had come from. clusterctl
reads YAML documents, validates each of them against a schema, merges them in a
fixed order and remembers the line every value was written on.

## Document kinds

Each document carries an `apiVersion` and a `kind`, so several of them may
share a file and a file may be read in any order.

| Kind | Scope | Replaces |
| --- | --- | --- |
| `Config` | One administrator | `source_me.sh` and the shell's environment |
| `Site` | One site | `domains.conf`, `hostnames.conf`, `networks.conf`, `ssh_config`, `var/sshuttle/*.conf` |
| `Cluster` | One cluster in a site | the per-cluster ClusterShell group files |
| `NodeInventory` | The nodes of a site | `node-attributes.conf` (genders), `node-inventory.csv`, `bootpaths.conf` |
| `Workstation` | The machine clusterctl runs on | local environment variables |

`clusterctl config schema KIND` prints the JSON Schema of a kind. Point an
editor at it and the fields, their types and their documentation are checked as
the file is typed:

```yaml
# yaml-language-server: $schema=https://gsi-hpc.github.io/clusterctl/schema/v1alpha1/site.json
```

## Where files are read from

`CLUSTERCTL_CONFIG` replaces the search path entirely and is a `PATH`-style
list of files and directories, most general first. Without it, clusterctl reads
`/etc/clusterctl` and then the user's configuration directory, usually
`~/.config/clusterctl`. A directory contributes its `.yaml` and `.yml` files in
name order. `--config` takes the same entries.

## The layers

Layers are applied in this order, each winning over the ones before it:

1. **defaults** — the built-in `internal/config/defaults.yaml`
2. **site** — the `Site` document the cluster names
3. **cluster** — the `Cluster` document the context names, and its `overrides`
4. **workstation** — the `Workstation` document for this machine, and its `overrides`
5. **context** — the `overrides` of the current context
6. **environment** — the documented environment variables
7. **flags** — `--set PATH=VALUE` and the command line

Mappings merge key by key. Sequences and scalars replace what was there: a list
of naming rules or of protected hosts only makes sense as a whole, and merging
two lists element by element produces something nobody wrote.

Each layer is validated against the schema of its kind **before** it is merged.
A typo is therefore reported where it was written rather than wherever the
merged value is eventually used:

```
$ clusterctl config validate
clusterctl: site.yaml is not valid:
  site.yaml:9:7: spec.hosts.login.forwardAgnet: unknown field "forwardAgnet"; did you mean "forwardAgent"?
```

## Provenance

Every merged value keeps the layer, file, line and column it came from.

```
$ clusterctl config explain fanout.max
FIELD   VALUE
path    fanout.max
value   24
layer   site
source  /etc/clusterctl/site.yaml:142:5

$ clusterctl config view --show-sources
PATH                    LAYER        VALUE
bmc.ipmi.driver         site         LAN_2_0
bmc.ipmi.ipmipowerPath  defaults     /usr/sbin/ipmipower
ssh.connectTimeout      workstation  15s
```

This is what makes a layered configuration usable: without it, a value that is
not what the file says sends an administrator through four documents by hand.

## Overrides

An `overrides` table addresses the merged tree by dotted path:

```yaml
contexts:
  - name: cluster2
    cluster: cluster2
    overrides:
      fanout.max: 6
      ssh.connectTimeout: 20s
```

An override table is applied by path and is deliberately **not** merged into
the tree as data as well, so `config view --show-sources` shows each override
once, under the path it affects.

## Numbers written with a leading zero

`mode: 0600` is read as the **string** `"0600"`.

YAML implementations disagree about whether a literal with a leading zero is
decimal or octal, and a file mode that silently becomes 384 only shows up on
the node. Fields that take a mode are declared as strings for the same reason.

## Paths

A path in a document is resolved against the directory of the `Site` document,
so a site can keep its configuration in version control and check it out
anywhere. A leading `~` is expanded. Absolute paths are left alone.

## Environment variables

| Variable | Effect |
| --- | --- |
| `CLUSTERCTL_CONFIG` | Replaces the configuration search path |
| `CLUSTERCTL_CONTEXT` | Selects the context, as `--context` does |
| `CLUSTERCTL_NODES` | The node set commands act on when `-n` is not given |
| `CLUSTERCTL_FANOUT` | `fanout.max` |
| `CLUSTERCTL_CONNECT_TIMEOUT` | `ssh.connectTimeout` |
| `CLUSTERCTL_COMMAND_TIMEOUT` | `fanout.commandTimeout` |
| `CLUSTERCTL_KNOWN_HOSTS` | `ssh.knownHostsFile` |
| `CLUSTERCTL_SSH_BINARY`, `CLUSTERCTL_SCP_BINARY` | The clients to run |
| `CLUSTERCTL_SSHUTTLE_BINARY` | The sshuttle to run |
| `CLUSTERCTL_PAGER`, `CLUSTERCTL_BROWSER` | The pager and browser to use |

A password is never one of these. It is named by the `Site` document as a
source, and `BMC_PASSWORD` is read only because a credential says `fromEnv:
BMC_PASSWORD`.

## State and cache

| Directory | Holds |
| --- | --- |
| `$XDG_STATE_HOME/clusterctl` | The generated `ssh_config`, the multiplexing sockets, the service processor certificate pins, the tunnel process id files |
| `$XDG_CACHE_HOME/clusterctl` | Fetched copies of remote files and resolved group listings |

Both are created with mode 0700. Nothing in either is authoritative: deleting
them costs one round trip.
