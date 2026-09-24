// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package v1alpha1

// Config is the per administrator document: which clusters this installation
// knows and which one commands act on by default. Several Config documents
// merge in the order CLUSTERCTL_CONFIG lists them.
type Config struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// CurrentContext names the context commands use unless --context says
	// otherwise.
	CurrentContext string `json:"currentContext,omitempty" yaml:"currentContext,omitempty" jsonschema:"description=Context used when --context is not given"`
	// Contexts binds a name to a cluster and the settings that differ per
	// administrator.
	Contexts []Context `json:"contexts,omitempty" yaml:"contexts,omitempty" jsonschema:"description=The clusters this installation can act on"`
}

// Context binds a name to a cluster and to the overrides that apply while it
// is current.
type Context struct {
	Name string `json:"name" yaml:"name" jsonschema:"required,description=Name given to --context"`
	// Cluster names the Cluster document this context acts on.
	Cluster string `json:"cluster" yaml:"cluster" jsonschema:"required,description=Name of the Cluster document"`
	// User is the remote account used for roles that do not name one.
	User string `json:"user,omitempty" yaml:"user,omitempty" jsonschema:"description=Default remote account"`
	// Overrides are applied on top of the site and cluster layers while this
	// context is current. Keys are dotted paths into the merged
	// configuration, for example "ssh.connectTimeout".
	Overrides map[string]any `json:"overrides,omitempty" yaml:"overrides,omitempty" jsonschema:"description=Dotted path overrides applied while this context is current"`
}

// Site describes one site: everything shared by the clusters that live in it.
type Site struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Spec     SiteSpec   `json:"spec" yaml:"spec"`
}

// SiteSpec holds the settings of a site.
type SiteSpec struct {
	// Domains maps a domain role to a DNS domain. The naming rules refer to
	// these by name, as {domains.hpc}.
	Domains map[string]string `json:"domains,omitempty" yaml:"domains,omitempty" jsonschema:"description=DNS domains by role: hpc, site, mgmt, mgmtHpc, infra"`
	// Naming turns a short node name into a host name and a BMC name.
	Naming NamingSpec `json:"naming,omitempty" yaml:"naming,omitempty"`
	// Hosts are the infrastructure roles: login, mgmt, install, mirror and
	// the rest of the hosts commands connect to by name.
	Hosts map[string]HostRole `json:"hosts,omitempty" yaml:"hosts,omitempty" jsonschema:"description=Infrastructure roles by name"`
	// Networks maps a network name to a CIDR, used by the tunnels and by the
	// BMC address checks.
	Networks map[string]string `json:"networks,omitempty" yaml:"networks,omitempty" jsonschema:"description=Named CIDRs"`
	// Credentials are the accounts used for BMCs and PDUs. A password is
	// never written here, only where to read it from: a Secret document is
	// one of the places.
	Credentials map[string]Credential `json:"credentials,omitempty" yaml:"credentials,omitempty" jsonschema:"description=Named credentials; passwords are referenced, never inlined"`
	// BMC configures out-of-band access.
	BMC BMCSpec `json:"bmc,omitempty" yaml:"bmc,omitempty"`
	// SSH configures the transport every remote command uses.
	SSH SSHSpec `json:"ssh,omitempty" yaml:"ssh,omitempty"`
	// Tunnels are the sshuttle profiles this site offers.
	Tunnels map[string]TunnelSpec `json:"tunnels,omitempty" yaml:"tunnels,omitempty" jsonschema:"description=sshuttle profiles by name"`
	// Safety bounds what destructive commands may do without asking.
	Safety SafetySpec `json:"safety,omitempty" yaml:"safety,omitempty"`
	// Fanout bounds parallel execution.
	Fanout FanoutSpec `json:"fanout,omitempty" yaml:"fanout,omitempty"`
	// Services describes the site services commands read and drive.
	Services ServicesSpec `json:"services,omitempty" yaml:"services,omitempty"`
}

// NamingSpec turns short node names into host names.
type NamingSpec struct {
	// Rules are tried in order; the first match wins. A rule with no match
	// block matches every name and should come last.
	Rules []NamingRule `json:"rules,omitempty" yaml:"rules,omitempty" jsonschema:"description=Naming rules, first match wins"`
	// BMCPrefix is available to the templates as {bmcPrefix}.
	BMCPrefix string `json:"bmcPrefix,omitempty" yaml:"bmcPrefix,omitempty" jsonschema:"description=Prefix available to templates as {bmcPrefix}"`
}

// NamingRule maps the node names it matches to host names.
type NamingRule struct {
	// Match selects the node names this rule applies to. An empty match
	// applies to every name.
	Match NamingMatch `json:"match,omitempty" yaml:"match,omitempty"`
	// FQDN is the template for the host name, for example
	// "{name}.{domains.hpc}".
	FQDN string `json:"fqdn,omitempty" yaml:"fqdn,omitempty" jsonschema:"description=Host name template, e.g. {name}.{domains.hpc}"`
	// BMC is the template for the BMC host name. A node whose rule has
	// none has no BMC name, and the bmc commands refuse it unless its
	// inventory entry sets bmcAddress.
	BMC string `json:"bmc,omitempty" yaml:"bmc,omitempty" jsonschema:"description=BMC host name template; without one the node needs a bmcAddress in the inventory"`
}

// NamingMatch selects node names by prefix or by regular expression.
type NamingMatch struct {
	Prefixes []string `json:"prefixes,omitempty" yaml:"prefixes,omitempty" jsonschema:"description=Short name prefixes this rule applies to"`
	Pattern  string   `json:"pattern,omitempty" yaml:"pattern,omitempty" jsonschema:"description=Regular expression the whole short name must match"`
}

// HostRole is one infrastructure host commands reach by role name.
type HostRole struct {
	// Host is the name to connect to. It should be the real host name, so
	// that the administrator's own ssh_config Host blocks still match.
	Host string `json:"host" yaml:"host" jsonschema:"required,description=Host name to connect to"`
	// User is the account to log in as.
	User string `json:"user,omitempty" yaml:"user,omitempty" jsonschema:"description=Remote account; defaults to the context user"`
	// ForwardAgent forwards the ssh agent to this role.
	ForwardAgent bool `json:"forwardAgent,omitempty" yaml:"forwardAgent,omitempty"`
	// ForwardX11 enables X11 forwarding for this role.
	ForwardX11 bool `json:"forwardX11,omitempty" yaml:"forwardX11,omitempty"`
	// ProxyJump names another role, or a host, to jump through.
	ProxyJump string `json:"proxyJump,omitempty" yaml:"proxyJump,omitempty" jsonschema:"description=Role name or host to jump through"`
	// ControlMaster keeps one connection open and reuses it. Use it for
	// gateways and hubs, never per node.
	ControlMaster bool `json:"controlMaster,omitempty" yaml:"controlMaster,omitempty"`
	// LegacyAlgorithms re-enables the ssh-rsa host key algorithm for hosts
	// whose sshd is too old for the current defaults.
	LegacyAlgorithms bool `json:"legacyAlgorithms,omitempty" yaml:"legacyAlgorithms,omitempty"`
	// Options are extra ssh_config keywords written into the generated
	// configuration for this role.
	Options map[string]string `json:"options,omitempty" yaml:"options,omitempty" jsonschema:"description=Extra ssh_config keywords for this role"`
	// Description is shown by node list and doctor.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Credential names an account and where its password comes from.
type Credential struct {
	Username string         `json:"username" yaml:"username" jsonschema:"required"`
	Password PasswordSource `json:"password,omitempty" yaml:"password,omitempty"`
}

// PasswordSource says where a password is read from. Exactly one field may be
// set; a literal password in the configuration is not a supported source.
type PasswordSource struct {
	// FromEnv reads the named environment variable.
	FromEnv string `json:"fromEnv,omitempty" yaml:"fromEnv,omitempty" jsonschema:"description=Environment variable holding the password"`
	// File reads the first line of a file.
	File string `json:"file,omitempty" yaml:"file,omitempty" jsonschema:"description=File whose first line is the password"`
	// AgeFile decrypts an age encrypted file with the configured identities.
	AgeFile string `json:"ageFile,omitempty" yaml:"ageFile,omitempty" jsonschema:"description=age encrypted file holding the password"`
	// SecretRef reads the password from a key of a sops encrypted Secret
	// document. It is decrypted only when the credential is used.
	SecretRef *SecretKeyRef `json:"secretRef,omitempty" yaml:"secretRef,omitempty" jsonschema:"description=Key of a sops encrypted Secret document holding the password"`
	// Command runs a helper and reads the password from its standard output.
	// A helper named by a relative path resolves against the Site document;
	// a bare name is looked up in PATH.
	Command []string `json:"command,omitempty" yaml:"command,omitempty" jsonschema:"description=Helper command printing the password; a relative path resolves against the Site document"`
	// Prompt asks the administrator on the terminal.
	Prompt bool `json:"prompt,omitempty" yaml:"prompt,omitempty" jsonschema:"description=Ask on the terminal"`
}

// BMCSpec configures out-of-band access to the node service processors.
type BMCSpec struct {
	// Credential names the entry in credentials used unless a vendor profile
	// names another.
	Credential string `json:"credential,omitempty" yaml:"credential,omitempty"`
	// Order is the transport preference. Redfish first matches current
	// firmware, where IPMI over LAN is off by default.
	Order   []string    `json:"order,omitempty" yaml:"order,omitempty" jsonschema:"description=Transport preference, e.g. [redfish, ipmi]"`
	IPMI    IPMISpec    `json:"ipmi,omitempty" yaml:"ipmi,omitempty"`
	Redfish RedfishSpec `json:"redfish,omitempty" yaml:"redfish,omitempty"`
	PDU     PDUSpec     `json:"pdu,omitempty" yaml:"pdu,omitempty"`
	// Vendors overrides the defaults for nodes whose vendor attribute
	// matches the key.
	Vendors map[string]VendorProfile `json:"vendors,omitempty" yaml:"vendors,omitempty" jsonschema:"description=Per vendor overrides keyed by the vendor node attribute"`
}

// IPMISpec configures the IPMI backends, which run on the management gateway
// rather than on the workstation.
type IPMISpec struct {
	// Via names the host role the backend runs on. Empty runs it locally.
	Via string `json:"via,omitempty" yaml:"via,omitempty" jsonschema:"description=Host role the IPMI tools run on"`
	// Backend selects ipmipower or ipmitool.
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty" jsonschema:"enum=ipmipower,enum=ipmitool"`
	// Driver is the FreeIPMI driver name, LAN_2_0 for anything current.
	Driver string `json:"driver,omitempty" yaml:"driver,omitempty"`
	// PasswordTransport decides how the password reaches the backend. The
	// default, file, writes it to a mode 0600 file on the gateway, so that
	// it never appears in the remote argv where ps would show it.
	PasswordTransport string `json:"passwordTransport,omitempty" yaml:"passwordTransport,omitempty" jsonschema:"enum=file,enum=stdin,enum=env"`
	// IpmipowerPath and IpmitoolPath override the backend locations.
	IpmipowerPath string   `json:"ipmipowerPath,omitempty" yaml:"ipmipowerPath,omitempty"`
	IpmitoolPath  string   `json:"ipmitoolPath,omitempty" yaml:"ipmitoolPath,omitempty"`
	Timeout       Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// RedfishSpec configures the native Redfish client.
type RedfishSpec struct {
	// TLSVerify turns certificate verification on. BMCs usually carry a self
	// signed certificate, so the default is to pin instead.
	TLSVerify *bool `json:"tlsVerify,omitempty" yaml:"tlsVerify,omitempty" jsonschema:"description=Verify the BMC certificate against the system roots"`
	// PinStore records the certificate fingerprint seen for each BMC and
	// refuses a silent change.
	PinStore string `json:"pinStore,omitempty" yaml:"pinStore,omitempty" jsonschema:"description=File recording the certificate fingerprint of each BMC"`
	// MinTLSVersion allows old firmware to be reached, "1.0" through "1.3".
	MinTLSVersion string `json:"minTlsVersion,omitempty" yaml:"minTlsVersion,omitempty" jsonschema:"enum=1.0,enum=1.1,enum=1.2,enum=1.3"`
	// Timeout bounds one request.
	Timeout Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// MaxConcurrent bounds how many BMCs are talked to at once.
	MaxConcurrent int `json:"maxConcurrent,omitempty" yaml:"maxConcurrent,omitempty"`
	// SystemPath is the Redfish path of the computer system, which differs
	// between vendors.
	SystemPath string `json:"systemPath,omitempty" yaml:"systemPath,omitempty"`
}

// PDUSpec configures rack power distribution units.
type PDUSpec struct {
	// NameFormat builds the PDU host name from the row and the rack, as a
	// printf format with two string verbs.
	NameFormat string `json:"nameFormat,omitempty" yaml:"nameFormat,omitempty" jsonschema:"description=printf format taking the row and the rack"`
	Domain     string `json:"domain,omitempty" yaml:"domain,omitempty"`
	User       string `json:"user,omitempty" yaml:"user,omitempty"`
	Credential string `json:"credential,omitempty" yaml:"credential,omitempty"`
}

// VendorProfile overrides BMC handling for one hardware vendor.
type VendorProfile struct {
	Credential string   `json:"credential,omitempty" yaml:"credential,omitempty"`
	Order      []string `json:"order,omitempty" yaml:"order,omitempty"`
	// ResetTypes lists the Redfish reset types the firmware accepts. When it
	// is empty the client asks the BMC instead of guessing.
	ResetTypes    []string `json:"resetTypes,omitempty" yaml:"resetTypes,omitempty"`
	SystemPath    string   `json:"systemPath,omitempty" yaml:"systemPath,omitempty"`
	MinTLSVersion string   `json:"minTlsVersion,omitempty" yaml:"minTlsVersion,omitempty"`
	TLSVerify     *bool    `json:"tlsVerify,omitempty" yaml:"tlsVerify,omitempty"`
}

// SSHSpec configures the OpenSSH client clusterctl drives.
type SSHSpec struct {
	// KnownHostsFile is the host key file the team keeps under version
	// control. Every connection is checked against it.
	KnownHostsFile string `json:"knownHostsFile,omitempty" yaml:"knownHostsFile,omitempty" jsonschema:"description=Host key file, usually versioned with the site configuration"`
	// Include lists the system ssh_config files the generated configuration
	// includes, so that crypto policies and site defaults keep applying.
	Include []string `json:"include,omitempty" yaml:"include,omitempty" jsonschema:"description=ssh_config files the generated file includes"`
	// StrictHostKeyChecking may be relaxed for a site that has not collected
	// its host keys yet. It is on by default.
	StrictHostKeyChecking *bool `json:"strictHostKeyChecking,omitempty" yaml:"strictHostKeyChecking,omitempty"`
	// ConnectTimeout and ConnectionAttempts bound one connection attempt.
	ConnectTimeout     Duration `json:"connectTimeout,omitempty" yaml:"connectTimeout,omitempty"`
	ConnectionAttempts int      `json:"connectionAttempts,omitempty" yaml:"connectionAttempts,omitempty"`
	// ServerAlive keeps an idle session from being dropped.
	ServerAliveInterval Duration `json:"serverAliveInterval,omitempty" yaml:"serverAliveInterval,omitempty"`
	ServerAliveCountMax int      `json:"serverAliveCountMax,omitempty" yaml:"serverAliveCountMax,omitempty"`
	// ControlPath is where multiplexed sockets live. Roles opt in with
	// controlMaster; nodes never do.
	ControlPath    string   `json:"controlPath,omitempty" yaml:"controlPath,omitempty"`
	ControlPersist Duration `json:"controlPersist,omitempty" yaml:"controlPersist,omitempty"`
	// SendEnv lists environment variables forwarded to the remote host.
	SendEnv []string `json:"sendEnv,omitempty" yaml:"sendEnv,omitempty"`
	// Options are extra ssh_config keywords applied to every host.
	Options map[string]string `json:"options,omitempty" yaml:"options,omitempty"`
	// LegacyKeyTypes re-enables ssh-rsa everywhere. Prefer setting it per
	// role. The generated file spells it PubkeyAcceptedKeyTypes, which every
	// supported OpenSSH understands.
	LegacyKeyTypes bool `json:"legacyKeyTypes,omitempty" yaml:"legacyKeyTypes,omitempty"`
	// Binary and ScpBinary override the client locations.
	Binary    string `json:"binary,omitempty" yaml:"binary,omitempty"`
	ScpBinary string `json:"scpBinary,omitempty" yaml:"scpBinary,omitempty"`
}

// TunnelSpec is one sshuttle profile.
type TunnelSpec struct {
	// Remote names the host role, or the host, sshuttle connects to.
	Remote string `json:"remote" yaml:"remote" jsonschema:"required,description=Host role or host sshuttle connects to"`
	// Subnets lists network names from the site, or CIDRs.
	Subnets []string `json:"subnets" yaml:"subnets" jsonschema:"required,description=Network names or CIDRs to route"`
	// Excludes are subnets or hosts kept off the tunnel, typically the
	// workstation itself.
	Excludes []string `json:"excludes,omitempty" yaml:"excludes,omitempty"`
	// DNS forwards DNS queries through the tunnel.
	DNS bool `json:"dns,omitempty" yaml:"dns,omitempty"`
	// User is the remote account.
	User string `json:"user,omitempty" yaml:"user,omitempty"`
	// Method selects the sshuttle firewall method.
	Method string `json:"method,omitempty" yaml:"method,omitempty"`
	// Options are extra sshuttle arguments.
	Options []string `json:"options,omitempty" yaml:"options,omitempty"`
	// Description is shown by tunnel status.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// SafetySpec bounds what a destructive command may do without being asked
// twice.
type SafetySpec struct {
	// ProtectedHosts are node set expressions that no destructive command
	// touches unless --force is given as well. An entry may name a node by
	// any of its names or addresses, or name a group, and must name only
	// nodes the inventory knows.
	ProtectedHosts []string `json:"protectedHosts,omitempty" yaml:"protectedHosts,omitempty" jsonschema:"description=Node sets that destructive commands refuse to touch"`
	// ConfirmAbove asks for the host count to be typed back when a
	// destructive command targets more than this many hosts. At zero the
	// count is always typed.
	ConfirmAbove int `json:"confirmAbove,omitempty" yaml:"confirmAbove,omitempty" jsonschema:"minimum=0,description=Ask for the count to be typed above this many hosts; 0 asks every time"`
	// SlurmAware refuses to power off a node that is running a job.
	SlurmAware *bool `json:"slurmAware,omitempty" yaml:"slurmAware,omitempty" jsonschema:"description=Check Slurm before power actions"`
	// PowerOnBatch and PowerOnStagger spread a power-on over time so that a
	// rack does not trip its breaker.
	PowerOnBatch   int      `json:"powerOnBatch,omitempty" yaml:"powerOnBatch,omitempty" jsonschema:"minimum=1,description=How many nodes to power on at once"`
	PowerOnStagger Duration `json:"powerOnStagger,omitempty" yaml:"powerOnStagger,omitempty"`
}

// FanoutSpec bounds parallel execution across nodes.
type FanoutSpec struct {
	// Max is how many nodes are worked on at once. Keep it conservative
	// when the connections go through a tunnel or a jump host.
	Max int `json:"max,omitempty" yaml:"max,omitempty" jsonschema:"description=Nodes contacted at once"`
	// ConnectTimeout bounds the connection, CommandTimeout the command. The
	// command timeout is enforced on the node with timeout(1), because
	// killing the local ssh does not stop the remote process.
	ConnectTimeout Duration `json:"connectTimeout,omitempty" yaml:"connectTimeout,omitempty"`
	CommandTimeout Duration `json:"commandTimeout,omitempty" yaml:"commandTimeout,omitempty"`
}

// ServicesSpec describes the site services the commands read and drive.
type ServicesSpec struct {
	DHCP   DHCPService   `json:"dhcp,omitempty" yaml:"dhcp,omitempty"`
	PXESrv PXESrvService `json:"pxesrv,omitempty" yaml:"pxesrv,omitempty"`
	TFTP   TFTPService   `json:"tftp,omitempty" yaml:"tftp,omitempty"`
	HTTP   HTTPService   `json:"http,omitempty" yaml:"http,omitempty"`
	Cinc   CincService   `json:"cinc,omitempty" yaml:"cinc,omitempty"`
	Mail   MailService   `json:"mail,omitempty" yaml:"mail,omitempty"`
	Fabric FabricService `json:"fabric,omitempty" yaml:"fabric,omitempty"`
	DNS    DNSService    `json:"dns,omitempty" yaml:"dns,omitempty"`
}

// DHCPService is the DHCP server whose configuration and log are read.
type DHCPService struct {
	// Role names the host role the server runs on.
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	// ConfigPath is the dhcpd configuration read for addresses and boot
	// files.
	ConfigPath string `json:"configPath,omitempty" yaml:"configPath,omitempty"`
	// CacheTTL is how long a fetched copy of that file is reused.
	CacheTTL Duration `json:"cacheTtl,omitempty" yaml:"cacheTtl,omitempty"`
	// LogPath is the system log searched for DHCP responses.
	LogPath string `json:"logPath,omitempty" yaml:"logPath,omitempty"`
	// Interface is the default interface for a traffic capture.
	Interface string `json:"interface,omitempty" yaml:"interface,omitempty"`
}

// PXESrvService is the PXE boot path service.
type PXESrvService struct {
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	// Root is where per node boot path links live, BootPath where the boot
	// configurations they point at live.
	Root     string `json:"root,omitempty" yaml:"root,omitempty"`
	BootPath string `json:"bootPath,omitempty" yaml:"bootPath,omitempty"`
	// StaticSuffix marks a boot path that survives the first request.
	StaticSuffix string `json:"staticSuffix,omitempty" yaml:"staticSuffix,omitempty"`
	LogPath      string `json:"logPath,omitempty" yaml:"logPath,omitempty"`
	// RepoPath is the git checkout git-pull refreshes.
	RepoPath string `json:"repoPath,omitempty" yaml:"repoPath,omitempty"`
}

// TFTPService is the TFTP server that serves GRUB and iPXE.
type TFTPService struct {
	Role     string `json:"role,omitempty" yaml:"role,omitempty"`
	Root     string `json:"root,omitempty" yaml:"root,omitempty"`
	GrubPath string `json:"grubPath,omitempty" yaml:"grubPath,omitempty"`
	LogPath  string `json:"logPath,omitempty" yaml:"logPath,omitempty"`
}

// HTTPService is the web server that hosts installation content.
type HTTPService struct {
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	Root string `json:"root,omitempty" yaml:"root,omitempty"`
	// BaseURL is how nodes reach that root.
	BaseURL string `json:"baseUrl,omitempty" yaml:"baseUrl,omitempty"`
}

// CincService is the configuration management the nodes run.
type CincService struct {
	// ArchivePath is where configuration archives are published on the HTTP
	// server.
	ArchivePath string `json:"archivePath,omitempty" yaml:"archivePath,omitempty"`
	// BaseCookbook is the git repository cloned into an archive.
	BaseCookbook string `json:"baseCookbook,omitempty" yaml:"baseCookbook,omitempty"`
	// RolesPath is the local directory holding the roles.
	RolesPath string `json:"rolesPath,omitempty" yaml:"rolesPath,omitempty"`
	// SoloConfigPath is the file on the node naming its archive and run
	// list.
	SoloConfigPath string `json:"soloConfigPath,omitempty" yaml:"soloConfigPath,omitempty"`
	// Binary is the client run on the node.
	Binary string `json:"binary,omitempty" yaml:"binary,omitempty"`
	// Secrets are the age encrypted files pushed to a node after it is
	// installed.
	Secrets []SecretFile `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

// SecretFile is one encrypted file and where it lands on the node. The
// plaintext is streamed to the node over stdin and never written to the
// workstation's disk.
type SecretFile struct {
	// Source is the age encrypted file, relative to the configuration
	// directory unless absolute. Exactly one of source and secretRef is set.
	Source string `json:"source,omitempty" yaml:"source,omitempty" jsonschema:"description=age encrypted file; exactly one of source and secretRef"`
	// SecretRef takes the content from a key of a sops encrypted Secret
	// document instead of a file of its own.
	SecretRef *SecretKeyRef `json:"secretRef,omitempty" yaml:"secretRef,omitempty" jsonschema:"description=Key of a sops encrypted Secret document holding the content; exactly one of source and secretRef"`
	// Target is the absolute path on the node.
	Target string `json:"target" yaml:"target" jsonschema:"required"`
	// Mode is the octal permission of the target, written as a string so
	// that "0600" is not read as a decimal number.
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty" jsonschema:"description=Octal mode written as a string, e.g. \"0600\""`
	// Owner and Group are set after the file is written.
	Owner string `json:"owner,omitempty" yaml:"owner,omitempty"`
	Group string `json:"group,omitempty" yaml:"group,omitempty"`
}

// SecretKeyRef names one key of a Secret document.
type SecretKeyRef struct {
	// Name is the metadata.name of the Secret document.
	Name string `json:"name" yaml:"name" jsonschema:"required,description=metadata.name of the Secret document"`
	// Key is the key under data or binaryData.
	Key string `json:"key" yaml:"key" jsonschema:"required,description=Key under data or binaryData of that document"`
}

// String renders the reference the way messages and tables print it.
func (r SecretKeyRef) String() string { return r.Name + "/" + r.Key }

// MailService is how a node warns its users before a reboot.
type MailService struct {
	Host string `json:"host,omitempty" yaml:"host,omitempty"`
	From string `json:"from,omitempty" yaml:"from,omitempty"`
	// Domain is appended to a local account to form the recipient.
	Domain string `json:"domain,omitempty" yaml:"domain,omitempty"`
	// SubjectPrefix is put in front of the subject.
	SubjectPrefix string `json:"subjectPrefix,omitempty" yaml:"subjectPrefix,omitempty"`
}

// FabricService is the InfiniBand fabric.
type FabricService struct {
	// Role names the host the fabric tools run on, which needs access to the
	// subnet manager.
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	// GUIDFormat builds an HCA GUID from a MAC address. The default,
	// mellanox, inserts 0300 between the two halves of the address.
	GUIDFormat string `json:"guidFormat,omitempty" yaml:"guidFormat,omitempty" jsonschema:"enum=mellanox"`
}

// DNSService is the resolver used for forward and reverse lookups.
type DNSService struct {
	// Server is the name server the dns commands ask instead of those in
	// /etc/resolv.conf: an address or host name with an optional port, 53
	// by default. An IPv6 address may be written with or without brackets.
	Server string `json:"server,omitempty" yaml:"server,omitempty"`
	// Timeout bounds one query.
	Timeout Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// Aliases are the names dns aliases resolves and reports.
	Aliases []string `json:"aliases,omitempty" yaml:"aliases,omitempty"`
}

// Cluster describes one cluster inside a site.
type Cluster struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta  `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Spec     ClusterSpec `json:"spec" yaml:"spec"`
}

// ClusterSpec holds what differs between the clusters of a site.
type ClusterSpec struct {
	// Site names the Site document this cluster belongs to.
	Site string `json:"site" yaml:"site" jsonschema:"required,description=Name of the Site document"`
	// Slurm configures access to the workload manager.
	Slurm SlurmSpec `json:"slurm,omitempty" yaml:"slurm,omitempty"`
	// Groups configures the node group sources.
	Groups GroupsSpec `json:"groups,omitempty" yaml:"groups,omitempty"`
	// BootPaths maps node sets to the PXE boot configuration they install
	// from. The first match wins, and a node matching two rules is an error.
	BootPaths []BootPathRule `json:"bootPaths,omitempty" yaml:"bootPaths,omitempty"`
	// Inventories names the NodeInventory documents that describe this
	// cluster's nodes. Empty means every inventory that is loaded.
	Inventories []string `json:"inventories,omitempty" yaml:"inventories,omitempty"`
	// Overrides are applied on top of the site layer for this cluster.
	Overrides map[string]any `json:"overrides,omitempty" yaml:"overrides,omitempty" jsonschema:"description=Dotted path overrides applied for this cluster"`
}

// SlurmSpec configures access to Slurm.
type SlurmSpec struct {
	// Role names the host role the Slurm clients run on.
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	// Organization is the default organisation for a new account.
	Organization string `json:"organization,omitempty" yaml:"organization,omitempty"`
	// DefaultAccount is the account a new user is associated with.
	DefaultAccount string `json:"defaultAccount,omitempty" yaml:"defaultAccount,omitempty"`
	// LookBack is the default accounting window for the job reports.
	LookBack Duration `json:"lookBack,omitempty" yaml:"lookBack,omitempty"`
}

// GroupsSpec configures where node groups come from.
type GroupsSpec struct {
	// DefaultSource is the source a bare @group reference uses.
	DefaultSource string `json:"defaultSource,omitempty" yaml:"defaultSource,omitempty"`
	// Sources maps a source name to its definition.
	Sources map[string]GroupSource `json:"sources,omitempty" yaml:"sources,omitempty"`
}

// GroupSource is one place group definitions come from.
type GroupSource struct {
	// Static holds the groups inline: a group name mapped to a node set
	// expression, which may itself refer to other groups.
	Static map[string]string `json:"static,omitempty" yaml:"static,omitempty"`
	// Attribute builds one group per value of a node attribute, so that the
	// inventory replaces the genders file.
	Attribute string `json:"attribute,omitempty" yaml:"attribute,omitempty" jsonschema:"description=Build groups from the values of this node attribute"`
	// Exec resolves groups by running commands, the way ClusterShell does.
	Exec *ExecGroupSource `json:"exec,omitempty" yaml:"exec,omitempty"`
	// CacheTTL is how long a resolved group is reused.
	CacheTTL Duration `json:"cacheTtl,omitempty" yaml:"cacheTtl,omitempty"`
}

// ExecGroupSource resolves groups by running commands on a host role. The
// commands are given as argument vectors, never as a shell string, and $GROUP
// and $NODE are substituted as arguments rather than expanded by a shell.
type ExecGroupSource struct {
	// Role names the host role the commands run on. Nothing runs on the
	// workstation, so it is required.
	Role string `json:"role" yaml:"role" jsonschema:"required"`
	// Map prints the nodes of the group named by $GROUP.
	Map []string `json:"map" yaml:"map" jsonschema:"required"`
	// All prints every node the source knows.
	All []string `json:"all,omitempty" yaml:"all,omitempty"`
	// List prints the available group names.
	List []string `json:"list,omitempty" yaml:"list,omitempty"`
	// Reverse prints the groups the node named by $NODE belongs to.
	Reverse []string `json:"reverse,omitempty" yaml:"reverse,omitempty"`
}

// BootPathRule maps a node set to a PXE boot configuration.
type BootPathRule struct {
	// Nodes is a node set expression.
	Nodes string `json:"nodes" yaml:"nodes" jsonschema:"required"`
	// Path is the boot configuration on the PXE server.
	Path string `json:"path" yaml:"path" jsonschema:"required"`
	// Static configures a boot path that survives the first request.
	Static bool `json:"static,omitempty" yaml:"static,omitempty"`
}

// NodeInventory describes the nodes of a site: what genders, the rack CSV and
// the boot path table used to hold.
type NodeInventory struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta        `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Spec     NodeInventorySpec `json:"spec" yaml:"spec"`
}

// NodeInventorySpec holds the node entries.
type NodeInventorySpec struct {
	// Defaults apply to every entry that does not set the field itself.
	Defaults NodeDefaults `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	// Nodes lists the entries. Later entries win over earlier ones for the
	// same node, so a general entry can be written first and refined after.
	Nodes []NodeEntry `json:"nodes,omitempty" yaml:"nodes,omitempty"`
}

// NodeDefaults are the attributes every node inherits.
type NodeDefaults struct {
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
}

// NodeEntry describes one node, or every node of a node set.
type NodeEntry struct {
	// Nodes is a node set expression. Fields that describe a single machine,
	// such as address and cid, may only be set when it names one node.
	Nodes string `json:"nodes" yaml:"nodes" jsonschema:"required,description=Node set expression this entry applies to"`
	// Attributes are the genders style key/value pairs used for grouping and
	// for selecting vendor profiles.
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	// Rack and Level place the node in the machine room.
	Rack  string `json:"rack,omitempty" yaml:"rack,omitempty"`
	Level string `json:"level,omitempty" yaml:"level,omitempty"`
	// Address is the node's IP address.
	Address string `json:"address,omitempty" yaml:"address,omitempty"`
	// BMCAddress is the address of its service processor. It wins over the
	// name the naming rules derive.
	BMCAddress string `json:"bmcAddress,omitempty" yaml:"bmcAddress,omitempty" jsonschema:"description=Host name or address of the service processor; wins over the name the naming rules derive"`
	// CID is the site asset identifier.
	CID string `json:"cid,omitempty" yaml:"cid,omitempty"`
	// MACs are the hardware addresses, used to derive the fabric GUID when
	// DHCP cannot be reached.
	MACs []string `json:"macs,omitempty" yaml:"macs,omitempty"`
	// BootPath overrides the cluster boot path rules for this node.
	BootPath string `json:"bootPath,omitempty" yaml:"bootPath,omitempty"`
}

// Workstation holds what is true of the machine clusterctl runs on rather
// than of the site.
type Workstation struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta      `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Spec     WorkstationSpec `json:"spec" yaml:"spec"`
}

// WorkstationSpec holds the local overrides.
type WorkstationSpec struct {
	// Host is this machine's name, available to the tunnel templates as
	// {workstation.host} so that a profile can exclude it.
	Host string `json:"host,omitempty" yaml:"host,omitempty"`
	// Addresses are named local addresses the tunnel profiles refer to.
	Addresses map[string]string `json:"addresses,omitempty" yaml:"addresses,omitempty"`
	// Browser and Pager override the programs used to open a BMC web
	// interface and to page long output.
	Browser string `json:"browser,omitempty" yaml:"browser,omitempty"`
	Pager   string `json:"pager,omitempty" yaml:"pager,omitempty"`
	// Identities are the age identities used to decrypt secrets.
	Identities []string `json:"identities,omitempty" yaml:"identities,omitempty"`
	// SopsKeyTypes are the kinds of master key a Secret document may be
	// encrypted to, age alone when unset. sops contacts the key management
	// service or Vault a file's metadata names with this machine's
	// credentials, and that metadata is not authenticated, so a kind is
	// used only when it is listed here.
	SopsKeyTypes []string `json:"sopsKeyTypes,omitempty" yaml:"sopsKeyTypes,omitempty" jsonschema:"enum=age,enum=pgp,enum=kms,enum=gcp_kms,enum=azure_kv,enum=hc_vault,enum=hckms"`
	// SshuttleBinary overrides where sshuttle is found.
	SshuttleBinary string `json:"sshuttleBinary,omitempty" yaml:"sshuttleBinary,omitempty"`
	// Overrides are applied on top of the cluster layer on this machine.
	Overrides map[string]any `json:"overrides,omitempty" yaml:"overrides,omitempty" jsonschema:"description=Dotted path overrides applied on this machine"`
}

// Secret holds values that must not be readable in version control. The file
// is encrypted with sops, with only data and binaryData encrypted, so that
// clusterctl can read the kind, the name and the keys without a key and
// decrypt only when a command uses one of the values:
//
//	sops --encrypt --encrypted-regex '^(data|binaryData)$' --in-place secrets.sops.yaml
//
// Other documents refer to a value with secretRef: {name, key}.
type Secret struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta `json:"metadata" yaml:"metadata" jsonschema:"required"`
	// Data are text values, such as passwords.
	Data map[string]string `json:"data,omitempty" yaml:"data,omitempty" jsonschema:"description=Text values by key; encrypted by sops"`
	// BinaryData are base64 encoded values, such as a munge key, decoded
	// before they are used.
	BinaryData map[string]string `json:"binaryData,omitempty" yaml:"binaryData,omitempty" jsonschema:"description=Base64 encoded values by key; encrypted by sops"`
	// Sops is the metadata sops writes: the recipients, the encrypted data
	// key and the message authentication code. It is never edited by hand.
	Sops map[string]any `json:"sops,omitempty" yaml:"sops,omitempty" jsonschema:"description=Written by sops; do not edit"`
}
