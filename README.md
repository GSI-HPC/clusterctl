# Cluster Tools

Collection of scripts used for cluster administration.
To add the configuration from this repository into your shell environment
**source the [`source_me.sh`](source_me.sh) file**:
```bash
source source_me.sh
```
This will
- load the shell environment**
- add the [`bin/`](bin/) sub-directory to your `PATH` and
- source all `*.sh` files contained in [`var/aliases/`](var/aliases/)

Some commands provided in this repository are related to cluster specific cinc
configuration and therefore expect that also the source_me.sh file of that
cluster's cinc_config repository has been sourced.

This is a list of the commands provided by this repository:
Command | Description
--------|---------------
`cluster-ipmi` | Access node BMCs IPMI interface
`cluster-pxesrv` | Provision nodes with PXE network boot and Anaconda/Kickstart[^8mP53]
`cluster-dhcp` | Verify the DHCP configuration on the servers
`cluster-cinc` | Configuration management, details in the corresponding repositories
[`expect-*`](docs/expect.md) | Wrapper scripts to automate password based SSH logins (mostly to work with BMCs[^dF34s])
`cluster-knownhosts` | Add the ssh public key of a host to site-config/ssh-known-hosts
`cluster-clush` | clush wrapper setting the known_host file local to this repo
`cluster-scp` | scp wrapper setting the known_host file local to this repo
`cluster-ssh` | ssh wrapper setting the known_host file local to this repo
`cluster-iblink` | check the state of IB connections for a node
`cluster-login` | Access cluster special nodes
`cluster-nodeset` | Remove, update or check ssh public keys in known_hosts file
`cluster-node-fqdn` | Print node or BMC host names with the domain selected by the node name prefix
`cluster-nodes-vendor` | Get hardware related information
`cluster-post-install` | Copy secrets to nodes after installation
`cluster-pxe-install` | Set the link to bootpath on the PXE server
`cluster-redfish` | Interact with a server's BMC using redfish
[`cluster-slurm-*`] | Wrapper scripts to get information from the Slurm cluster
`mlx-hca` | Wrapper for mlx* command (interaction with Mellanox HCA)
`sshuttles` | Wrapper for sshuttle

[^8mP53]: CLuster Provisioning, Anaconda/Kickstart  
<https://git.example.com/hpc/cluster/cluster-kickstart>

[^dF34s]: Cluster BMC Configuration  
<https://git.example.com/hpc/cluster/bmc-configuration>


## Installation

Set the boot path of a compute node:

```bash
cluster-pxe-install $nodeset
# if a node is not supported add it to site-config/bootpaths.conf

# it might be necessary to set HCA configuration to support link up on boot for PXE
clush -l root -w $nodeset -- 'mlxconfig -y -d mlx5_0 set KEEP_LINK_UP_ON_BOOT_P1=1'

# set boot target to PXE and reset the node
export BMC_PASSWORD=             # for the `admin` or the `Administrator` user
cluster-redfish -n $nodeset command -- boot_override --target Pxe --reset
```

After successful installation add the SSH host public key to the known_hosts
file [`site-config/ssh-known-hosts`](site-config/ssh-known-hosts).

```bash
# add host keys to the known hosts in the repository
cluster-knownhosts $nodeset

# or use the `-o` option to overwrite the key for existing known hosts 
cluster-knownhosts -o $nodeset
```

## Login

SSH wrappers using [`site-config/ssh_config`](site-config/ssh_config) and
[`site-config/ssh-known-hosts`](site-config/ssh-known-hosts):

```bash
cluster-ssh exe0001 'uname -r'
cluster-scp /path/to/file root@exe0001:/path/to/file
cluster-clush -w 'wlm01,dbm01' uname -r

# access service nodes
cluster-login -w 'systemctl status slurmctld'
cluster-login -d 'systemctl status mariadb'
```

## Post Configuration

`cluster-post-install` deploys secrets and start the services

Install the SSH host key common to all submit nodes:

```bash
cluster-scp $CLUSTER_CINC_CONFIG_PATH/etc/ssh/sshd_config.d/99-hostkey.conf root@$node:/etc/ssh/sshd_config.d/
# write the private key and derive the public key from it
age -d $CLUSTER_CINC_CONFIG_PATH/etc/ssh/ssh_host_ed25519_key.age \
      | cluster-ssh $node 'cat >/etc/ssh/ssh_host_ed25519_key &&
          ssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key >/etc/ssh/ssh_host_ed25519_key.pub'
```


## Configuration

All cluster-specific configuration lives in the [`site-config/`](site-config/) directory
and is sourced by [`source_me.sh`](source_me.sh).

### Config Files

| File | Purpose |
|------|---------|
| `site-config/domains.conf` | Domain suffixes (`CLUSTER_DOMAIN_SITE`, `_HPC`, `_MGMT`, `_MGMT_HPC`, `_INFRA`) and the node name prefixes selecting them (`CLUSTER_NODE_PREFIXES_HPC`, `_SITE`) |
| `site-config/hostnames.conf` | Hostname constants (`CLUSTER_HOST_LOGIN`, `_SSH_PROXY`, `_MGMT_GATEWAY`, `_DHCP`, `_IB_GATEWAY`, `_POOL`, `_IFS`, `_MIRROR`, `_INSTALL`, `_SMTP`, `_DNS`, `_TFTP`, `CLUSTER_NAME`, `CLUSTER_SLURM_ORG`, `_SLURM_PARTITIONS`, `_TFTP_GRUB_PATH`, `_TFTP_ROOT`, `_KICKSTART_PATH`, `_PDU_NAME_FORMAT`, `_RACK_PATTERN`, `_NODE_PREFIX`, `_BMC_USER`, `_BMC_PREFIX`) |
| `site-config/networks.conf` | Network topology (`CLUSTER_NET_IPMI`, and `CLUSTER_NET_CLUSTER1`, `_SITE_EXT`, `_INTERNAL`, `_SYS02_1`…`_4` for sshuttle) |
| `site-config/bootpaths.conf` | Boot path data (read via `BOOTPATH_FILE`; not sourced as shell) |
| `site-config/ssh-known-hosts` | SSH host keys (default of `$CLUSTER_LOGIN_SSH_KNOWN_HOSTS`, overridden by `$CLUSTER_KNOWN_HOSTS_FILE`) |
| `site-config/ssh_config` | SSH client config used by `cluster-ssh`, `cluster-scp`, `cluster-clush` |
| `site-config/node-attributes.conf` | Node attributes for ClusterShell |
| `site-config/node-inventory.csv` | Rack/serial/vendor data (referenced as `$RACKS_FILE`) |
| `site-config/clush-*.conf` | ClusterShell group sources (symlinked from `var/clustershell/groups.conf.d/`) |

### Environment Overrides

Every variable from the `site-config/` config files can be overridden by setting
the matching environment variable before sourcing:

```bash
export CLUSTER_HOST_LOGIN=mylogin.hpc.example.com
source source_me.sh
```

### Sensitive Data

The `site-config/` directory is tracked in Git and contains cluster-specific
configuration (hostnames, domains, network topology, SSH keys, boot paths,
node inventory). Restrict repository access accordingly.

### Node Names

`cluster-node-fqdn` holds the host naming conventions used by `cluster-ipmi`,
`cluster-redfish`, `cluster-nodeset`, `cluster-nodes-vendor`, `cluster-pxesrv`
and `CLUSTER_NODES`. A short node name starting with one of the prefixes in
`CLUSTER_NODE_PREFIXES_HPC` or `CLUSTER_NODE_PREFIXES_SITE` gets the domain
`CLUSTER_DOMAIN_HPC` or `CLUSTER_DOMAIN_SITE`. Its BMC is named
`<node>.$CLUSTER_DOMAIN_MGMT_HPC` or `$CLUSTER_BMC_PREFIX<node>.$CLUSTER_DOMAIN_MGMT`.

```bash
cluster-node-fqdn 'exe[0001-0002]'          # exe[0001-0002].hpc.example.org
cluster-node-fqdn --bmc 'exe[0001-0002]'    # exe[0001-0002].mgmt.hpc.example.org
```

### ClusterShell Groups

`var/clustershell/groups.conf` loads group sources from
`var/clustershell/groups.conf.d/*.conf`, which are symlinks to
`site-config/clush-*.conf`. To add a cluster, create
`site-config/clush-<name>.conf` and link it:

```bash
ln -s ../../../site-config/clush-<name>.conf var/clustershell/groups.conf.d/<name>.conf
```

### Sshuttle Templates

The files in `var/sshuttle/` hold one `sshuttle` argument per line.
`sshuttles start <name>` expands `${VAR}` and `${VAR:-default}` references
from the environment. An argument that expands to an empty string is dropped,
along with a preceding `--exclude`/`-x`, so unset `IP_EXCL_*` variables are
skipped. An empty `--remote` host is an error.

Addresses excluded from the tunnels are set per workstation:
`IP_EXCL_SSH_PROXY`, `IP_EXCL_SYS01_HOST`, `IP_EXCL_SYS02_HOST`,
`IP_EXCL_LOGIN_1`, `IP_EXCL_LOGIN_2`, `IP_EXCL_CLUSTER1`, `IP_EXCL_LOCAL_HOST`,
`IP_EXCL_LOCAL_NET` and `IP_EXCL_LIBVIRT_NET`.

### Tool Configuration Matrix

| Tool | Config files | Key variables | Templates |
|------|-------------|---------------|-----------|
| `cluster-login` | `hostnames.conf`, `domains.conf`, `site-config/ssh-known-hosts` | `CLUSTER_KNOWN_HOSTS_FILE`, `CLUSTER_LOGIN_SSH_KNOWN_HOSTS`, `CLUSTER_HOST_LOGIN`, `_DNS`, `_MGMT_GATEWAY`, `_IFS`, `_MIRROR`, `_INSTALL`, `_SSH_PROXY` | — |
| `cluster-ssh` / `cluster-scp` / `cluster-clush` | `site-config/ssh_config`, `site-config/ssh-known-hosts` | `CLUSTER_KNOWN_HOSTS_FILE`, `CLUSTER_LOGIN_SSH_KNOWN_HOSTS` | — |
| `cluster-clush` | `var/clustershell/`, `site-config/node-attributes.conf` | `CLUSTERSHELL_CFGDIR`, `CLUSTER_NODES` | ClusterShell configs |
| `cluster-ipmi` | `domains.conf`, `networks.conf` | `BMC_PASSWORD`, `CLUSTER_NET_IPMI`, `CLUSTER_DOMAIN_SITE`, `_MGMT_HPC`, `_INFRA`, `CLUSTER_PDU_NAME_FORMAT`, `CLUSTER_VPN_CHECK`, `CLUSTER_VPN_COMMAND` | — |
| `cluster-redfish` | `domains.conf` | `CLUSTER_DOMAIN_MGMT_HPC`, `_MGMT` | — |
| `cluster-node-fqdn` | `domains.conf`, `hostnames.conf` | `CLUSTER_NODE_PREFIXES_HPC`, `_SITE`, `CLUSTER_DOMAIN_HPC`, `_SITE`, `_MGMT`, `_MGMT_HPC`, `CLUSTER_BMC_PREFIX` | — |
| `cluster-pxesrv` | `hostnames.conf`, `domains.conf` | `CLUSTER_PXESRV_SERVER`, `CLUSTER_DNS`, `_PXESRV_ROOT`, `_PXESRV_BOOT_PATH` | — |
| `cluster-pxe-install` | `site-config/bootpaths.conf` | `BOOTPATH_FILE` | `BOOTPATH_FILE` |
| `cluster-dhcp` | `hostnames.conf` | `CLUSTER_DHCP_SERVER`, `CLUSTER_HOST_MGMT_GATEWAY` | — |
| `cluster-iblink` | `hostnames.conf` | `CLUSTER_HOST_MGMT_GATEWAY`, `_IB_GATEWAY`, `_POOL`, `_LOGIN` | — |
| `cluster-cinc` | — | `CLUSTER_BASE_COOKBOOK`, `CLUSTER_CINC_CONFIG_PATH` | — |
| `cluster-knownhosts` | `site-config/ssh-known-hosts` | `CLUSTER_LOGIN_SSH_KNOWN_HOSTS`, `CLUSTER_KNOWN_HOSTS_FILE` | — |
| `cluster-nodeset` | `site-config/ssh-known-hosts`, `domains.conf` | `CLUSTER_KNOWN_HOSTS_FILE`, `CLUSTER_LOGIN_SSH_KNOWN_HOSTS`, `CLUSTER_DOMAIN_HPC`, `_SITE` | — |
| `cluster-nodes-vendor` | `domains.conf` | `CLUSTER_DOMAIN_HPC`, `_SITE` | — |
| `cluster-reboot-node` | `hostnames.conf`, `domains.conf` | `CLUSTER_HOST_SMTP`, `CLUSTER_DOMAIN_SITE`, `CLUSTER_NAME` | — |
| `cluster-post-install` | `site-config/` (all) | `CLUSTER_CINC_CONFIG_PATH` | — |
| `sshuttles` | `var/sshuttle/*.conf`, `networks.conf` | `SSHUTTLE_CONFIGS`, `CLUSTER_HOST_SSH_PROXY`, `_MGMT_GATEWAY`, `_LOGIN`, `CLUSTER_NET_*`, `IP_EXCL_*` | `var/sshuttle/*.conf` |
| `cluster-dns-aliases` | `hostnames.conf`, `domains.conf` | `CLUSTER_HOST_POOL`, `CLUSTER_DOMAIN_HPC`, `CLUSTER_SLURM_PARTITIONS` | — |
| `cluster-tftp` | `hostnames.conf`, `domains.conf` | `CLUSTER_HOST_TFTP`, `CLUSTER_TFTP_GRUB_PATH`, `CLUSTER_TFTP_ROOT`, `CLUSTER_KICKSTART_PATH`, `CLUSTER_DOMAIN_SITE` | — |
| `cluster-racks` | `site-config/node-inventory.csv` | `RACKS_FILE`, `CLUSTER_RACK_PATTERN` | — |
| `expect-*` | — | CLI `$1` (node), `$2` (password) | — |

