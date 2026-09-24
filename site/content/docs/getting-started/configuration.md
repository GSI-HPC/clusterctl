---
title: Write a configuration
weight: 2
---

Configuration is YAML documents, each carrying an `apiVersion` and a `kind`.
Several may share a file.

## Start from a scaffold

```console
$ clusterctl config init --site lab --cluster alpha --domain hpc.example.org --user alice_adm
FILE                                           KIND           NAME
/home/alice/.config/clusterctl/cluster.yaml    Cluster        alpha
/home/alice/.config/clusterctl/config.yaml     Config
/home/alice/.config/clusterctl/inventory.yaml  NodeInventory  lab
/home/alice/.config/clusterctl/site.yaml       Site           lab

4 files written. The comments in them say what to fill in; then check the result:
  clusterctl config validate
  clusterctl doctor
```

This is the least configuration that resolves: one context, a login node, a
naming rule, a host key file and an empty node inventory. Every flag is
optional; without them the files carry the names of the example site. Fill in
your nodes and whatever else the comments point at, and check the result:

```console
$ $EDITOR ~/.config/clusterctl/inventory.yaml
$ clusterctl config validate
```

Without a directory, the files go where clusterctl reads configuration from:
the directory `CLUSTERCTL_CONFIG` or `--config` names, or else your own
configuration directory. Give a directory to write somewhere else, such as a
new one in the repository the site documents are to live in. Directories that
are missing are created. Your own directory is read together with
`/etc/clusterctl`, so when `/etc/clusterctl` holds a configuration already,
such as your team's, `config init` asks for a directory rather than write a
second one that would change what the team's resolves to. `--dry-run` lists the files without writing them:

```console
$ clusterctl config init ./site-config --dry-run
```

`config init` writes only into an empty directory and never overwrites a
file, so running it twice changes nothing. A directory that holds anything, a
`README` or a `.git` directory included, is refused: run `config init` before
`git init`, or write into a subdirectory. So is a directory another user owns
or others can write, even an empty one.

For everything the scaffold leaves out, `examples/site/` ships with the
release and is a complete, working configuration. Take from it what your site
needs.

## The six kinds

| Kind | What it says |
| --- | --- |
| `Config` | Which clusters this installation knows and which one is current |
| `Site` | Domains, naming, host roles, networks, credentials, ssh, tunnels, safety, services |
| `Cluster` | Which site it belongs to, its Slurm, its groups, its boot paths |
| `NodeInventory` | The nodes: attributes, racks, addresses, boot paths |
| `Workstation` | What is true of this machine rather than of the site |
| `Secret` | Passwords and keys, encrypted with sops |

## A minimal site

Enough to log in and run commands. It is what `config init --user alice_adm`
writes, without the comments and with the documents in one file:

```yaml
apiVersion: clusterctl/v1alpha1
kind: Site
metadata:
  name: example
spec:
  domains:
    hpc: hpc.example.org

  naming:
    rules:
      - fqdn: "{name}.{domains.hpc}"

  hosts:
    login:
      host: login.hpc.example.org
      description: Cluster login node, where the Slurm clients run

  ssh:
    knownHostsFile: ssh-known-hosts

  safety:
    protectedHosts: []
---
apiVersion: clusterctl/v1alpha1
kind: Cluster
metadata:
  name: cluster1
spec:
  site: example
  inventories: [example]
  slurm:
    role: login
  groups:
    defaultSource: inventory
    sources:
      inventory:
        attribute: class
---
apiVersion: clusterctl/v1alpha1
kind: NodeInventory
metadata:
  name: example
spec:
  nodes: []
---
apiVersion: clusterctl/v1alpha1
kind: Config
currentContext: cluster1
contexts:
  - name: cluster1
    cluster: cluster1
    user: alice_adm
```

## Let your editor check it

```console
$ clusterctl config schema Site > ~/.config/clusterctl/site.schema.json
```

Then put this at the top of the file:

```yaml
# yaml-language-server: $schema=./site.schema.json
```

Fields, types and their documentation are now checked as you type. The schemas
are also published: `https://gsi-hpc.github.io/clusterctl/schema/v1alpha1/site.json`.

## Where files are read from

Without `CLUSTERCTL_CONFIG`, clusterctl reads `/etc/clusterctl` and then your
own configuration directory, usually `~/.config/clusterctl`. A directory
contributes its `.yaml` files in name order.

```console
$ export CLUSTERCTL_CONFIG=/srv/site-config:~/.config/clusterctl
$ clusterctl --config ./examples/site config validate
```

Both take files and directories, most general first. Every place they name
has to exist: a misspelled one is an error, not a file quietly left out. Of the
two directories read without them, one that is missing is skipped.

Configuration names programs clusterctl runs, so like `~/.ssh/config` it has to
be yours or root's, and nobody else may be able to write the files or the
directory they are in. Otherwise clusterctl refuses to read them and says
which one to `chmod go-w`. Share a site directory through version control or
keep it owned by root, not writable by a group.

{{< callout type="info" >}}
Keep the `Site`, `Cluster`, `NodeInventory` and sops encrypted `Secret`
documents in version control with the host key file next to them. Paths in a document resolve against the
directory of the `Site` document, so the checkout moves as a unit.
{{< /callout >}}

## Naming rules

Naming rules turn a short node name into a host name and a service processor
name. The first rule that matches wins, and a rule with no `match` matches
everything, so it goes last.

```yaml
naming:
  rules:
    - match:
        prefixes: [exe, sub, wlm, dbm]
      fqdn: "{name}.{domains.hpc}"
      bmc:  "{name}.{domains.mgmtHpc}"
    - match:
        pattern: '^srv\d+$'
      fqdn: "{name}.{domains.infra}"
      bmc:  "{bmcPrefix}{name}.{domains.mgmt}"
    - fqdn: "{name}.{domains.site}"
      bmc:  "{bmcPrefix}{name}.{domains.mgmt}"
```

Check them without connecting to anything:

```console
$ clusterctl node fqdn -n 'exe[1-4]'
exe[0001-0004].hpc.example.org
$ clusterctl node fqdn -n 'exe[1-4]' --bmc
exe[0001-0004].mgmt.hpc.example.org
```

A name that already carries a domain is left as written, so you can always
name a host precisely. Host names are not case sensitive, so `WLM01` is
lowercased and gets the rule of `wlm01`.

A `pattern` must match the whole short name: `gpu\d+` does not claim
`login-gpu01`.

The service processor of a node is named only by the `bmc` template of the
rule that matches it. There is no fallback to the node's own name, because the
BMC password would then be sent to the node itself. A node whose rule has no
`bmc` template, or whose service processor cannot be named by a rule at all,
gets its address in the inventory:

```yaml
nodes:
  - nodes: exe0003
    bmcAddress: 10.9.0.77
```

`bmcAddress` always wins over a derived name, which is only as good as its DNS
record. For a name with a domain, the service processor is derived only when
the domain is the one the rules give the node; an IP address names no service
processor.

## Passwords

A password is never written in the configuration. A credential says where to
read one from:

```yaml
credentials:
  bmc:
    username: admin
    password:
      fromEnv: BMC_PASSWORD      # an environment variable
  bmc-vault:
    username: admin
    password:
      ageFile: secrets/bmc.age   # an age encrypted file
  pdu:
    username: admin
    password:
      prompt: true               # ask on the terminal
```

`file:` and `command:` also exist. Exactly one source per credential.

## Which layer won

Values come from seven layers: built-in defaults, the site, the cluster, this
workstation, the current context, the environment, and the flags. Each wins
over the ones before it.

```console
$ clusterctl config explain fanout.max
FIELD   VALUE
path    fanout.max
value   24
layer   site
source  /etc/clusterctl/site.yaml:142:5

$ clusterctl config view --show-sources | head
```

This is the command to reach for when a value is not what a file says.
