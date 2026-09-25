<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Configuration

The shell toolkit carried about ninety flat environment variables, data in five
formats, and no way to tell which of them a value had come from. clusterctl
reads YAML documents, validates each of them against a schema, merges them in a
fixed order and remembers the line every value was written on.

## Document kinds

Each document carries an `apiVersion` and a `kind`, so several of them may
share a file and a file may be read in any order.

A document other than a `Config` is defined once. A second `Site`, `Cluster`,
`NodeInventory`, `Workstation` or `Secret` of the same name is an error that
names both, rather than the later one replacing the earlier: the files of a
directory are read in name order, so a stale `site_old.yaml` would otherwise
take the place of `site.yaml`, protected hosts and all. Contexts are the
exception: a context of a later `Config` document replaces the one of the same
name, so that a personal file can adjust a shared one. One document that names
a context twice is an error.

| Kind | Scope | Replaces |
| --- | --- | --- |
| `Config` | One administrator | `source_me.sh` and the shell's environment |
| `Site` | One site | `domains.conf`, `hostnames.conf`, `networks.conf`, `ssh_config`, `var/sshuttle/*.conf` |
| `Cluster` | One cluster in a site | the per-cluster ClusterShell group files |
| `NodeInventory` | The nodes of a site | `node-attributes.conf` (genders), `node-inventory.csv`, `bootpaths.conf` |
| `Workstation` | The machine clusterctl runs on | local environment variables |
| `Secret` | Values encrypted with sops | the `.age` files next to the configuration |

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
name order, leaving out hidden files such as `.sops.yaml`. `--config` takes the
same entries.

A place missing from the built-in search path is skipped, because a site may
use only one of them. An entry of `--config` or `CLUSTERCTL_CONFIG` that does
not exist is a usage error: it is most likely misspelled, and without it the
command would resolve to another context.

Configuration names programs clusterctl runs on the workstation, such as
`ssh.binary` and a password command, so it is held to what OpenSSH holds
`~/.ssh/config` to. A configuration file, the directory it is read from, and
the directory holding a file named on its own have to be owned by the user
running clusterctl or by root and must not be writable by their group or by
anyone else; otherwise the command refuses to run. A directory with the sticky
bit set, such as `/tmp`, may hold a file named on its own. A site directory
several administrators share is therefore owned by root, or kept in version
control and checked out by each administrator; a group-writable one is refused.

## Starting a configuration

`clusterctl config init [DIR]` writes the least configuration that resolves: a
`Config` with one context, a `Site` with a login node, a `Cluster` and an
empty `NodeInventory`, one document to a file. Without `DIR` it writes where
configuration is read from: the directory `--config` or `CLUSTERCTL_CONFIG`
names, or else the user's configuration directory. When either names several
places it asks for `DIR`. So it does when it would write into the user's
configuration directory and another directory of the search path,
`/etc/clusterctl`, holds configuration already: the two are read together, and
a second complete configuration would replace the documents of the same name
or move the current context, and with either the protected hosts of the first. Directories that are missing are created. Flags fill in the names, the
domain, the login node and the remote account; the comments in each file say
what is left to fill in.

- **Checked before it is written.** The files are loaded, validated and
  resolved in memory the way files read from disk are, so a value that would
  not resolve is refused before anything is written. A name with a space in
  it or a domain with a trailing dot is refused as well, although the schema
  would take it. Every name is written double quoted, so that a cluster
  called `1e3` or a site called `08` or `yes` reads back as the same string
  with every YAML reader.
- **The cluster is pinned to its site's nodes.** Its `Cluster` lists the
  site's `NodeInventory` under `inventories`. A cluster that lists none takes
  the inventories of its own site. A `NodeInventory` does not say which site
  it belongs to, so that is every inventory that is loaded while one site is
  loaded; with several sites loaded, such a cluster is refused, by every
  command and by `config validate`, rather than handed the nodes of another
  site to resolve through its own naming rules.
- **Only into an empty directory.** A directory that holds anything, a hidden
  file or a `.git` directory included, is refused, with `--dry-run` as well.
  To start a site repository, write into an empty subdirectory of it, or run
  `config init` first and `git init` afterwards.
  [ADR 0017](adr/0017-config-init-into-an-empty-directory.md) says why.
- **Only into a directory of its own.** A directory another user owns, or one
  its group or anyone can write, is refused even when it is empty, and so is a
  new directory inside one, unless that one has the sticky bit set, as `/tmp`
  has. Configuration written there could not be read anyway.
- **Nothing is overwritten.** Each file is created exclusively, and when one
  cannot be written the ones written before it are removed again.
- **The template is not the example.** `examples/site/` shows every kind and
  most fields in use; the scaffold holds only what a first `config validate`,
  `doctor` and `node fqdn` need. Workstation and Secret documents are left
  out, because neither is needed to resolve and both hold what only the
  administrator knows.

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

Every merged value keeps the layer, file, line and column it came from. A
value of a context, its `user` and its overrides, keeps the line of the
`Config` document that defined it; one from the environment names the
variable, and one from the command line names `--set` or `--fanout`.

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

The tables are typed as plain maps, so the schema of the kind they are written
in cannot check them. Every override is checked instead against the schema of
the merged configuration, in every context and not only the current one, and
so is every `--set` and environment variable:

- **Paths are matched with their case.** `safety.protectedhosts` is refused
  with a suggestion, and is not taken for `safety.protectedHosts` by the case
  insensitive JSON decoder.
- **Values have the type the path expects.** `safety: null` or
  `safety: [wlm01]` is refused rather than erasing the section.
- **A mapping merges key by key**, as it does in a document. `safety:
  {confirmAbove: 4}` and `safety.confirmAbove: 4` mean the same, and neither
  touches `safety.protectedHosts`.
- **Limits keep their range.** `fanout.max` has to be at least 1, like
  `safety.powerOnBatch`. A `0` or a negative value is refused wherever it is
  written, `--fanout` included, rather than read as the default of 16; only
  a value that nothing sets falls back to it.

```
$ clusterctl config validate
clusterctl: config.yaml is not valid:
  config.yaml:21:7: safety.protectedhosts: unknown field "protectedhosts"; did you mean "protectedHosts"?
```

A key with a dot in it, such as a static group `rack.R01`, stays one key when
the layers merge. An override cannot address it by path, because the dot
separates the keys of a path.

## Numbers written with a leading zero

`mode: 0600` is read as the **string** `"0600"`.

YAML implementations disagree about whether a literal with a leading zero is
decimal or octal, and a file mode that silently becomes 384 only shows up on
the node. Fields that take a mode are declared as strings for the same reason.

## Paths

A path in a document is resolved against the directory of the `Site` document,
so a site can keep its configuration in version control and check it out
anywhere. A path given in the environment, `CLUSTERCTL_KNOWN_HOSTS`, or with
`--set` is resolved against the working directory instead, the way the shell
it was typed in reads it. A leading `~` is expanded. Absolute paths are left
alone.

## Environment variables

| Variable | Effect |
| --- | --- |
| `CLUSTERCTL_CONFIG` | Replaces the configuration search path |
| `CLUSTERCTL_CONTEXT` | Selects the context, as `--context` does |
| `CLUSTERCTL_NODES` | The node set commands act on when neither `-n` nor a node set argument is given; an empty `-n` is an error, never a fall-back to it |
| `CLUSTERCTL_FANOUT` | `fanout.max` |
| `CLUSTERCTL_CONNECT_TIMEOUT` | `ssh.connectTimeout` |
| `CLUSTERCTL_COMMAND_TIMEOUT` | `fanout.commandTimeout` |
| `CLUSTERCTL_KNOWN_HOSTS` | `ssh.knownHostsFile` |
| `CLUSTERCTL_SSH_BINARY`, `CLUSTERCTL_SCP_BINARY` | The clients to run |
| `CLUSTERCTL_SSHUTTLE_BINARY` | The sshuttle to run |
| `CLUSTERCTL_SOPS_BINARY` | The sops that decrypts Secret documents |
| `CLUSTERCTL_BROWSER` | The browser to use |

A password is never one of these. It is named by the `Site` document as a
source, and `BMC_PASSWORD` is read only because a credential says `fromEnv:
BMC_PASSWORD`.

## Secrets

A password or a file pushed onto the nodes can live in a `Secret` document,
encrypted with [sops](https://getsops.io);
[ADR 0013](adr/0013-sops-secret-documents.md) records why. The document is
alone in its file, and only its values are encrypted:

```yaml
# secrets.sops.yaml
apiVersion: clusterctl/v1alpha1
kind: Secret
metadata:
  name: example
data:            # text, used as written
  bmc-password: hunter2
  pin: "0600"    # quoted, or sops would store the number 384
binaryData:      # base64, decoded before use
  munge-key: 8qL5N...==
```

```console
$ sops --encrypt --encrypted-regex '^(data|binaryData)$' --in-place secrets.sops.yaml
```

A `.sops.yaml` next to it writes the rule down once, together with the keys
the file is encrypted to; `examples/site/.sops.yaml` is one. After that,
`sops secrets.sops.yaml` edits the file and `sops updatekeys` re-keys it.

The other documents refer to a value by the Secret's name and a key:

| Field | Instead of |
| --- | --- |
| `credentials.NAME.password.secretRef` | `ageFile`, or any other password source |
| `services.cinc.secrets[].secretRef` | `source` |

```yaml
credentials:
  bmc:
    username: admin
    password:
      secretRef: {name: example, key: bmc-password}
```

Nothing is decrypted while the configuration loads. The names and keys of a
Secret, and its sops metadata, are readable without a key, so a reference to
one that does not exist is reported at its line, and a command that uses no
secret needs neither a key nor sops. When a command does use one, the `sops`
command decrypts the document into memory
([ADR 0019](adr/0019-decrypt-with-the-sops-command.md)). It has to be sops
3.10.0 or later, the first that reads the file from standard input and opens
it with an OpenSSH key; `clusterctl doctor` checks it whenever the
configuration holds a Secret, and `secrets check` names the one it uses. sops
is looked up in `PATH`, as the ssh client is, unless `workstation.sopsBinary`
or `CLUSTERCTL_SOPS_BINARY` names another.

The keys of `workstation.identities` are tried first. sops opens them itself,
by path: an age identity file through `SOPS_AGE_KEY_FILE`, an OpenSSH key
through `SOPS_AGE_SSH_PRIVATE_KEY_FILE`, one file per run of sops and only a
file that holds a recipient of the Secret, so no key is copied anywhere. Such
a run gets an environment of `PATH`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR`
and `TZ` alone, `HOME`, `XDG_CONFIG_HOME` and `GNUPGHOME` pointed at an empty
directory, and no controlling terminal: it finds no key it was not given, and
cannot ask for a passphrase. At a terminal, sops then looks for a key itself,
as `sops decrypt` would at the same prompt: `SOPS_AGE_KEY_FILE` and its other
variables, `~/.config/sops/age/keys.txt`, `~/.ssh/id_ed25519`, a PGP agent,
the credentials of a cloud KMS or Vault, trying age and PGP keys before any
service. It gets clusterctl's environment then, except `SOPS_KEYSERVICE`,
`SOPS_ENABLE_LOCAL_KEYSERVICE`, `SOPS_CONFIG` and `SOPS_DECRYPTION_ORDER`,
which would change how the file is read. Without a terminal, under MCP or in a
script, only `workstation.identities` are used, because sops' own search can
run a program or ask for a passphrase.

sops is always given an empty configuration (`--config /dev/null`), so no
`.sops.yaml` above the working directory changes how a Secret is read; the
`.sops.yaml` next to the documents is for editing them.

The keys a file is encrypted to are listed in its sops metadata, which the
message authentication code does not cover: anyone who can write the file can
add a Vault address there, and sops would send this machine's Vault token to
it. A Secret is therefore refused when it names a kind of key the workstation
does not trust, before any key is tried. Only age is trusted unless
`workstation.sopsKeyTypes` lists more:

```yaml
kind: Workstation
spec:
  identities: [~/.ssh/id_ed25519]
  sopsKeyTypes: [age, pgp]   # age, pgp, kms, gcp_kms, azure_kv, hc_vault, hckms
  sopsBinary: /opt/sops/bin/sops   # sops in PATH when unset
```

Every kind of key sops supports opens a Secret once it is trusted, since sops
itself tries them. A sops mapping with a field clusterctl does not know is
refused as well: it may name a kind of key a newer sops tries and this list
cannot check.

The loader also refuses what sops would not have written, and what it did
write but clusterctl will not read:

```
$ clusterctl config validate
clusterctl: secrets.sops.yaml is not valid:
  secrets.sops.yaml:9:5: data.pdu-password: the value is not encrypted: it was added without sops; edit the file with "sops secrets.sops.yaml" instead
```

- **A value that is not text.** sops stores an unquoted `0600`, `007`, `true`
  or `2001-12-14` as a number, a boolean or a date, and hands back `384`, `7`,
  `true` or `2001-12-14T00:00:00Z`. Quote such a value in the plaintext. The
  type sops records is not authenticated either, so a value whose type is
  anything but a string is refused before it is decrypted.
- **`mac_only_encrypted`.** Under it the message authentication code covers
  only the encrypted values, and the kind and the name could be changed
  without a key.

When a Secret cannot be decrypted, the error says which keys were tried and
nothing of the values: what sops printed is passed on, escaped, only when it
could not open the data key, and then it lists the keys it tried. A file whose
message authentication code does not match is reported as changed without
sops; any other failure of sops is reported by its exit status alone, since
its message can quote a decrypted value.

`clusterctl secrets check --decrypt` proves this workstation opens every Secret
without printing any of it.

## State and cache

| Directory | Holds |
| --- | --- |
| `$XDG_STATE_HOME/clusterctl` | The generated `ssh_config-*` files, one per configuration, the multiplexing sockets, the service processor certificate pins, the tunnel process id files |
| `$XDG_CACHE_HOME/clusterctl` | Fetched copies of remote files and resolved group listings |

`$XDG_STATE_HOME` defaults to `~/.local/state` and `$XDG_CACHE_HOME` to
`~/.cache`; a relative value is ignored, as the XDG specification requires.
Without a home directory and without an absolute value, as under `env -i` or
in a system unit without `User=`, a command that reads the configuration
refuses to run: there is no fallback to a shared directory such as `/tmp`.

Both are created with mode 0700. One that exists already has to be a
directory, not a link to one, owned by the user running clusterctl and not
writable by its group or anyone else; otherwise every command refuses to run
and `doctor` names it. Whoever could write it could replace the `ssh_config`
that `ssh -F` reads. Nothing in either is authoritative: deleting them costs
one round trip.
