---
title: Write a configuration
weight: 2
---

Configuration is YAML documents, each carrying an `apiVersion` and a `kind`.
Several may share a file.

```console
$ mkdir -p ~/.config/clusterctl
$ cp examples/site/*.yaml ~/.config/clusterctl/
$ $EDITOR ~/.config/clusterctl/site.yaml
$ clusterctl config validate
```

`examples/site/` ships with the release and is a complete, working
configuration. Copy it and replace the values.

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

Enough to log in and run commands:

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
      - match:
          prefixes: [exe, sub, wlm]
        fqdn: "{name}.{domains.hpc}"
        bmc: "{name}.{domains.mgmtHpc}"
      - fqdn: "{name}.{domains.hpc}"

  hosts:
    login:
      host: login.hpc.example.org
      description: Where the Slurm clients run

  ssh:
    knownHostsFile: ssh-known-hosts
---
apiVersion: clusterctl/v1alpha1
kind: Cluster
metadata:
  name: cluster1
spec:
  site: example
  slurm:
    role: login
  groups:
    defaultSource: inventory
    sources:
      inventory:
        attribute: class
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

Both take files and directories, most general first.

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

A name that already carries a domain is left exactly as written, so you can
always name a host precisely.

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
