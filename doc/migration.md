<!-- SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de> -->
<!-- SPDX-License-Identifier: LGPL-3.0-or-later -->

# Moving a site to clusterctl

clusterctl is a clean break. There are no `cluster-*` shims, no `source_me.sh`
and no importer for the old configuration files: the old flags are the
inconsistency the rewrite exists to remove, and carrying them would carry that
too.

This is the map from what a site had to what it needs.

## Configuration

| Old file | New home |
| --- | --- |
| `site-config/domains.conf` | `Site` → `spec.domains` and `spec.naming.rules` |
| `site-config/hostnames.conf` | `Site` → `spec.hosts` (one entry per role) |
| `site-config/networks.conf` | `Site` → `spec.networks` |
| `site-config/ssh_config` | generated; per-role settings go in `spec.hosts.<role>` |
| `site-config/ssh-known-hosts` | unchanged; name it in `spec.ssh.knownHostsFile` |
| `site-config/node-attributes.conf` (genders) | `NodeInventory` → `spec.nodes[].attributes` |
| `site-config/node-inventory.csv` | `NodeInventory` → `rack`, `level`, `address`, `cid` |
| `site-config/bootpaths.conf` | `Cluster` → `spec.bootPaths` |
| `site-config/clush-*.conf` | `Cluster` → `spec.groups.sources` |
| `var/sshuttle/*.conf` | `Site` → `spec.tunnels` |
| `var/aliases/*.sh` | gone; the commands do this |

`examples/site/` holds a complete configuration built from exactly these files.
Copy it and replace the values.

### Environment variables

Almost every `CLUSTER_*` variable becomes a configuration field:

| Old | New |
| --- | --- |
| `CLUSTER_DOMAIN_HPC` and friends | `spec.domains.hpc` and friends |
| `CLUSTER_HOST_LOGIN`, `CLUSTER_HOST_MGMT_GATEWAY`, … | `spec.hosts.<role>.host` |
| `CLUSTER_NODES` | `CLUSTERCTL_NODES`, or `-n` |
| `CLUSTER_LOGIN_USER` | the context's `user` |
| `CLUSTER_KNOWN_HOSTS_FILE` | `CLUSTERCTL_KNOWN_HOSTS`, or `spec.ssh.knownHostsFile` |
| `BMC_USER`, `BMC_PASSWORD` | `spec.credentials.<name>`, with `password.fromEnv: BMC_PASSWORD` |
| `CLUSTER_PDU_NAME_FORMAT` | `spec.bmc.pdu.nameFormat` |
| `CLUSTER_CINC_ROLES`, `CLUSTER_HTTP_*` | `spec.services.cinc`, `spec.services.http` |

## Commands

| Old | New |
| --- | --- |
| `cluster-login` | `clusterctl login [ROLE]` |
| `cl`, `cli`, `cla` | `clusterctl login`, `login -X mgmt`, `login -A` |
| `cluster-ssh`, `cluster-scp` | `clusterctl login`, `clusterctl copy` |
| `cluster-clush`, `rush`, `crush` | `clusterctl exec -n …` |
| `cluster-nodeset` | `clusterctl node select`, `clusterctl hostkey …` |
| `cluster-node-fqdn` | `clusterctl node fqdn [--bmc]` |
| `cluster-knownhosts` | `clusterctl hostkey refresh` |
| `cluster-nodes-vendor` | `clusterctl node hw` |
| `cluster-racks` | `clusterctl node rack` |
| `nodeattr` | `clusterctl node attrs` |
| `sshuttles` | `clusterctl tunnel` |
| `cluster-ipmi power`, `chassis` | `clusterctl bmc power` |
| `cluster-ipmi bmc-name`, `bmc-ip` | `clusterctl node fqdn --bmc`, `clusterctl dns lookup --bmc` |
| `cluster-ipmi bmc-ping` | `clusterctl bmc ping` |
| `cluster-ipmi web` | `clusterctl bmc web` |
| `cluster-ipmi pdu` | `clusterctl pdu shell` |
| `cluster-redfish get/post` | `clusterctl bmc redfish get/post` |
| `cluster-redfish power` | `clusterctl bmc power` |
| `cluster-dhcp mac/ip/filename/iden` | `clusterctl dhcp hosts` |
| `cluster-dhcp response`, `tcpdump` | `clusterctl dhcp log`, `clusterctl dhcp capture` |
| `cluster-pxesrv boot/remove/status` | `clusterctl boot set/unset/status` |
| `cluster-pxesrv list/logs/git-pull` | `clusterctl boot list/log/sync` |
| `cluster-pxe-install` | `clusterctl boot set` (the rules decide the path) |
| `cluster-tftp-grub-install` | `clusterctl boot grub set` |
| `cluster-post-install` | `clusterctl secrets push` |
| `cluster-cinc config/run/solo` | `clusterctl cinc config/run` |
| `cluster-iblink state/counters` | `clusterctl fabric state/counters` |
| `mlx-hca link/cable/config` | `clusterctl hca link/cable/config` |
| `cluster-slurm-nodes *` | `clusterctl slurm node *` |
| `cluster-slurm-jobs *` | `clusterctl slurm job *` |
| `cluster-slurm-accounts *` | `clusterctl slurm account *` |
| `cluster-slurm-users *` | `clusterctl slurm user *` |
| `cluster-dns-aliases` | `clusterctl dns aliases` |
| `cluster-reboot-node` | `clusterctl bmc power soft`, or `exec -- shutdown -r` |
| `expect-ssh`, `expect-scp`, `expect-ssh-sudo` | nothing; use keys, or `bmc redfish` |

## Flags that changed meaning

The old tools used the same letter for different things. These are now fixed
across every command:

| Flag | Meant | Now means |
| --- | --- | --- |
| `-n` | a node in `cluster-login`, a nodeset elsewhere | always a node set |
| `-d` | debug in most tools, the database host in `cluster-login` | nothing; use `-o` and the messages |
| `-D` | debug, or date, or database | nothing |
| `-w` | the workload manager host | nothing; `login wlm` |
| `-r` | root, but only before `-i` | always root, in any order |
| `-o` | overwrite in `cluster-knownhosts` | always the output format |
| `-y` | did not exist | always "confirm in advance" |

## Things that no longer exist

- **`expect` scripts.** They sent the password whether or not a prompt
  appeared. Use keys; where a service processor only speaks password, use
  `clusterctl bmc redfish`.
- **`netcat`, `tac`, `tree`, `reset`, `sshpass`, `ip2host`.** Required by the
  old tools, and either unused or replaced.
- **A session-wide node set that commands silently fall back to.** Set
  `CLUSTERCTL_NODES` if you want one; nothing guesses.

## Suggested order

1. Start from `clusterctl config init`, then fill in `Site`, `Cluster` and
   `NodeInventory` from the old files; `examples/site/` shows every field in
   use.
2. `clusterctl config validate`, then `clusterctl config view --show-sources`
   and read it against the old `.conf` files.
3. `clusterctl doctor --remote`.
4. `clusterctl node select '@compute'` and compare with `nodeset -f`.
5. Read-only commands next: `node list`, `slurm node list`, `bmc status`,
   `dhcp hosts`.
6. Destructive commands last, with `--dry-run` first.
