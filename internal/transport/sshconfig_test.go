// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// serverClient returns a client that reaches the test server through a role
// named "srv", with the given host key file and extra settings.
func serverClient(t *testing.T, srv *testServer, dir, knownHosts string, spec v1alpha1.SSHSpec) *transport.Client {
	t.Helper()
	return transport.New(transport.Options{
		SSH: spec,
		Roles: map[string]v1alpha1.HostRole{
			"srv": {Host: srv.Host, Options: srv.Options()},
		},
		StateDir:       filepath.Join(dir, "state"),
		KnownHostsFile: knownHosts,
	})
}

func runOnServer(t *testing.T, c *transport.Client, srv *testServer) *transport.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.Run(ctx, transport.Target{Name: "srv", Host: srv.Host, Role: "srv"},
		transport.Request{Argv: []string{"true"}})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	return res
}

// The site's host key file is the only trust anchor. A key vouched for by a
// global file or a KnownHostsCommand from an included configuration, such as
// the one a FreeIPA client installs, must not let a host in whose key the
// site file does not hold.
func TestHostKeysAreCheckedAgainstTheSiteFileAlone(t *testing.T) {
	t.Parallel()
	srv := startServer(t)

	t.Run("the site file lets the right key in", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		known := writeFile(t, filepath.Join(dir, "site-known-hosts"), srv.KnownHosts+"\n")
		res := runOnServer(t, serverClient(t, srv, dir, known, v1alpha1.SSHSpec{}), srv)
		if res.Failed() || !strings.Contains(res.Stdout, serverOutput) {
			t.Fatalf("the connection failed although the site file holds the key: %+v", res)
		}
	})

	includes := map[string]func(dir, trusted string) string{
		"a global host key file": func(_, trusted string) string {
			return "GlobalKnownHostsFile " + trusted + "\n"
		},
		"a known hosts command": func(_, trusted string) string {
			return "KnownHostsCommand /bin/cat " + trusted + "\n"
		},
		"host key records in DNS": func(_, trusted string) string {
			return "VerifyHostKeyDNS yes\nGlobalKnownHostsFile " + trusted + "\n"
		},
	}
	for name, include := range includes {
		t.Run(name+" in an include does not override the site file", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			known := writeFile(t, filepath.Join(dir, "site-known-hosts"), srv.otherKnownHosts(t)+"\n")
			trusted := writeFile(t, filepath.Join(dir, "trusted"), srv.KnownHosts+"\n")
			inc := writeFile(t, filepath.Join(dir, "include.conf"), include(dir, trusted))

			res := runOnServer(t, serverClient(t, srv, dir, known, v1alpha1.SSHSpec{Include: []string{inc}}), srv)
			if !res.Failed() {
				t.Fatalf("the host was let in on a key the site file does not hold:\n%s", res.Stdout)
			}
			if !strings.Contains(res.Stderr, "Host key verification failed") {
				t.Errorf("the connection failed for another reason than the host key:\n%s", res.Stderr)
			}
		})
	}
}

func TestConfigurationWithoutAHostKeyFileIsRefused(t *testing.T) {
	t.Parallel()
	// Without a site file ssh falls back to ~/.ssh/known_hosts, which is
	// not the trust anchor the site keeps.
	c := transport.New(transport.Options{StateDir: t.TempDir()})
	if _, err := c.ConfigPath(); err == nil || !strings.Contains(err.Error(), "knownHostsFile") {
		t.Errorf("ConfigPath = %v, want a refusal naming ssh.knownHostsFile", err)
	}
}

// Two configurations used by processes of the same user must never share a
// file: ssh reads the file again for every connection, so one process would
// otherwise connect with the trust settings the other wrote.
func TestEachConfigurationGetsItsOwnFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	strict := false
	client := func(known string, spec v1alpha1.SSHSpec) *transport.Client {
		return transport.New(transport.Options{
			SSH:            spec,
			StateDir:       dir,
			KnownHostsFile: known,
		})
	}

	first, err := client("/site/one/known_hosts", v1alpha1.SSHSpec{}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client("/other/site/known_hosts", v1alpha1.SSHSpec{StrictHostKeyChecking: &strict}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two configurations share %s", first)
	}
	after, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("writing the second configuration changed the first:\n%s", after)
	}
	if !strings.Contains(string(after), "StrictHostKeyChecking yes") {
		t.Errorf("the first configuration lost strict checking:\n%s", after)
	}

	again, err := client("/site/one/known_hosts", v1alpha1.SSHSpec{}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("the same configuration was written to %s and %s", first, again)
	}
}

// A description may span lines, but nothing in it may become a directive.
func TestAMultiLineDescriptionStaysAComment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := transport.New(transport.Options{
		Roles: map[string]v1alpha1.HostRole{
			"a": {Host: "a.example.org", Description: "Management gateway\nStrictHostKeyChecking no"},
		},
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "known_hosts"),
	})
	path, err := c.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "StrictHostKeyChecking no") {
			t.Errorf("a line of the description became a directive:\n%s", data)
		}
	}
	if got := resolved(t, path, "a.example.org")["stricthostkeychecking"]; got != "true" {
		t.Errorf("ssh resolved StrictHostKeyChecking %q, want true", got)
	}
}

func TestControlCharactersInValuesAreRefused(t *testing.T) {
	t.Parallel()
	role := func(r v1alpha1.HostRole) transport.Options {
		return transport.Options{Roles: map[string]v1alpha1.HostRole{"a": r}, KnownHostsFile: "/k"}
	}
	tests := map[string]transport.Options{
		"a host":               role(v1alpha1.HostRole{Host: "a.example.org\nStrictHostKeyChecking no"}),
		"a user":               role(v1alpha1.HostRole{Host: "a.example.org", User: "root\nStrictHostKeyChecking no"}),
		"a jump host":          role(v1alpha1.HostRole{Host: "a.example.org", ProxyJump: "gw.example.org\nUser root"}),
		"a role option key":    role(v1alpha1.HostRole{Host: "a.example.org", Options: map[string]string{"Port\nUser": "22"}}),
		"a role option value":  role(v1alpha1.HostRole{Host: "a.example.org", Options: map[string]string{"Port": "22\nUser root"}}),
		"a carriage return":    role(v1alpha1.HostRole{Host: "a.example.org", Description: "one\rtwo"}),
		"a global option":      {SSH: v1alpha1.SSHSpec{Options: map[string]string{"Compression": "yes\nUser root"}}, KnownHostsFile: "/k"},
		"an environment name":  {SSH: v1alpha1.SSHSpec{SendEnv: []string{"LANG\nUser root"}}, KnownHostsFile: "/k"},
		"an include":           {SSH: v1alpha1.SSHSpec{Include: []string{"/etc/ssh/ssh_config\nUser root"}}, KnownHostsFile: "/k"},
		"the host key file":    {KnownHostsFile: "/k\nStrictHostKeyChecking no"},
		"the default user":     {DefaultUser: "alice\nUser root", KnownHostsFile: "/k"},
		"a leading dash user":  {DefaultUser: "-oProxyCommand=touch", KnownHostsFile: "/k"},
		"a control path":       {SSH: v1alpha1.SSHSpec{ControlPath: "/tmp/cm\n"}, KnownHostsFile: "/k"},
		"a quote in the known": {KnownHostsFile: `/k"x`},
		"an expansion":         {KnownHostsFile: "/k/${HOME}"},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := opts.Validate(); err == nil {
				t.Errorf("%s with a control character was accepted", name)
			}
			opts.StateDir = t.TempDir()
			if _, err := transport.New(opts).ConfigPath(); err == nil {
				t.Errorf("a configuration was generated from %s with a control character", name)
			}
		})
	}
}

// A ControlMaster from an included file must not reach the compute nodes: a
// connection through an existing master checks no host key at all.
func TestControlMasterIsOffUnlessARoleAsksForIt(t *testing.T) {
	t.Parallel()
	srv := startServer(t)

	// A master opened by the administrator's own ssh, which trusts the
	// server's real key.
	sockets, err := os.MkdirTemp("/tmp", "cm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockets) })
	socket := filepath.Join(sockets, "master")
	dir := t.TempDir()
	trusted := writeFile(t, filepath.Join(dir, "trusted"), srv.KnownHosts+"\n")
	own := writeFile(t, filepath.Join(dir, "own.conf"),
		"Host *\n  UserKnownHostsFile "+trusted+"\n  StrictHostKeyChecking yes\n  BatchMode yes\n")
	master := exec.Command(requireSSH(t), "-F", own, "-p", srvPort(srv), "-fN",
		"-o", "ControlMaster=yes", "-o", "ControlPath="+socket, srv.Host)
	if out, err := master.CombinedOutput(); err != nil {
		t.Fatalf("opening a master failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ssh", "-F", own, "-o", "ControlPath="+socket, "-O", "exit", srv.Host).Run()
	})

	known := writeFile(t, filepath.Join(dir, "site-known-hosts"), srv.otherKnownHosts(t)+"\n")
	inc := writeFile(t, filepath.Join(dir, "include.conf"),
		"ControlMaster auto\nControlPath "+socket+"\n")
	res := runOnServer(t, serverClient(t, srv, dir, known, v1alpha1.SSHSpec{Include: []string{inc}}), srv)
	if !res.Failed() {
		t.Fatalf("a master from an included file carried a connection past the site file:\n%s", res.Stdout)
	}
}

func srvPort(srv *testServer) string { return srv.Options()["Port"] }

func TestControlMasterSocketsAreKeptApartPerTrustSetting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	strict := false
	paths := map[string]string{}
	for name, opts := range map[string]transport.Options{
		"strict":      {KnownHostsFile: "/site/known_hosts"},
		"relaxed":     {KnownHostsFile: "/site/known_hosts", SSH: v1alpha1.SSHSpec{StrictHostKeyChecking: &strict}},
		"other sites": {KnownHostsFile: "/other/known_hosts"},
	} {
		opts.StateDir = dir
		opts.Roles = map[string]v1alpha1.HostRole{
			"gw":   {Host: "gw.example.org", ControlMaster: true},
			"node": {Host: "node.example.org"},
		}
		path, err := transport.New(opts).ConfigPath()
		if err != nil {
			t.Fatal(err)
		}
		gw := resolved(t, path, "gw.example.org")
		if gw["controlmaster"] != "auto" {
			t.Errorf("%s: the gateway does not multiplex: %q", name, gw["controlmaster"])
		}
		paths[name] = gw["controlpath"]
		if node := resolved(t, path, "node.example.org"); node["controlmaster"] != "false" {
			t.Errorf("%s: a node multiplexes: %q", name, node["controlmaster"])
		}
	}
	if paths["strict"] == paths["relaxed"] || paths["strict"] == paths["other sites"] {
		t.Errorf("masters opened under different trust settings share a socket: %q", paths)
	}
}

func TestIncludedControlMasterDoesNotReachNodes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	inc := writeFile(t, filepath.Join(dir, "include.conf"), "ControlMaster auto\nControlPath /tmp/%C\n")
	path, err := transport.New(transport.Options{
		SSH:            v1alpha1.SSHSpec{Include: []string{inc}},
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "known_hosts"),
	}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved(t, path, "node.example.org")["controlmaster"]; got != "false" {
		t.Errorf("ControlMaster from an include reached a node: %q", got)
	}
}

// ADR 0017 puts the configuration under ~/Library/Application Support on
// macOS, so a path with a space is the default there.
func TestAHostKeyFileWithASpaceIsReadWhole(t *testing.T) {
	t.Parallel()
	srv := startServer(t)
	dir := t.TempDir()
	known := writeFile(t, filepath.Join(dir, "Application Support", "clusterctl", "ssh-known-hosts"), srv.KnownHosts+"\n")
	res := runOnServer(t, serverClient(t, srv, dir, known, v1alpha1.SSHSpec{}), srv)
	if res.Failed() {
		t.Fatalf("the host key file was not read: %+v", res)
	}
}

// A role block matches the real host name, so whatever it says applies to
// every connection to that host. The role's account must not leak onto a
// node of the same name.
func TestARoleAccountDoesNotReachANodeOfTheSameName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := transport.New(transport.Options{
		Roles:          map[string]v1alpha1.HostRole{"wlm": {Host: "wlm01.example.org", User: "slurmadm"}},
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "known_hosts"),
	})
	path, err := c.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved(t, path, "wlm01.example.org")["user"]; got == "slurmadm" {
		t.Errorf("a connection to the node wlm01 logs in with the role's account %q", got)
	} else if me, err := user.Current(); err == nil && got != me.Username {
		t.Errorf("a node logs in as %q, want the local account %q", got, me.Username)
	}

	// The role itself still gets its account, on the command line.
	args, err := c.Args(transport.Target{Name: "wlm", Host: "wlm01.example.org", Role: "wlm"}, transport.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "slurmadm@wlm01.example.org") {
		t.Errorf("Args = %q, want the role's account", args)
	}
}

func TestProxyJumpCyclesAreRefused(t *testing.T) {
	t.Parallel()
	tests := map[string]map[string]v1alpha1.HostRole{
		"two roles": {
			"mgmt": {Host: "mgmt.example.org", ProxyJump: "dhcp"},
			"dhcp": {Host: "dhcp.example.org", ProxyJump: "mgmt"},
		},
		"a role and itself": {
			"mgmt": {Host: "mgmt.example.org", ProxyJump: "mgmt"},
		},
		"through a literal host that is a role host": {
			"mgmt": {Host: "mgmt.example.org", ProxyJump: "root@dhcp.example.org"},
			"dhcp": {Host: "dhcp.example.org", ProxyJump: "mgmt"},
		},
		"through the second hop of a list": {
			"a": {Host: "a.example.org", ProxyJump: "b"},
			"b": {Host: "b.example.org", ProxyJump: "gw.example.org,c"},
			"c": {Host: "c.example.org", ProxyJump: "a"},
		},
	}
	for name, roles := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := transport.Options{Roles: roles, KnownHostsFile: "/k", StateDir: t.TempDir()}
			err := opts.Validate()
			if err == nil || !strings.Contains(err.Error(), "cycle") {
				t.Errorf("Validate = %v, want a cycle reported", err)
			}
			if _, err := transport.New(opts).ConfigPath(); err == nil {
				t.Error("a configuration with a jump cycle was generated")
			}
		})
	}
}

func TestProxyJumpResolvesEveryHop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	roles := map[string]v1alpha1.HostRole{
		"mgmt":    {Host: "mgmt-gw.example.org"},
		"install": {Host: "installer.example.org", User: "root"},
		"a":       {Host: "a.example.org", ProxyJump: "mgmt,install"},
		"b":       {Host: "b.example.org", ProxyJump: "admin@mgmt"},
		"c":       {Host: "c.example.org", ProxyJump: "installer.example.org:2222"},
	}
	path, err := transport.New(transport.Options{
		Roles: roles, StateDir: dir, KnownHostsFile: filepath.Join(dir, "k"), DefaultUser: "alice",
	}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{
		"a.example.org": "alice@mgmt-gw.example.org,root@installer.example.org",
		"b.example.org": "admin@mgmt-gw.example.org",
		"c.example.org": "root@installer.example.org:2222",
	} {
		if got := resolved(t, path, host)["proxyjump"]; got != want {
			t.Errorf("%s jumps through %q, want %q", host, got, want)
		}
	}
}

func TestSmallerGeneratorDefects(t *testing.T) {
	t.Parallel()
	refused := map[string]transport.Options{
		"roles sharing a host with different settings": {Roles: map[string]v1alpha1.HostRole{
			"a": {Host: "gw.example.org", ControlMaster: true},
			"b": {Host: "gw.example.org", ProxyJump: "c"},
			"c": {Host: "c.example.org"},
		}},
		"a trust keyword in a role option": {Roles: map[string]v1alpha1.HostRole{
			"a": {Host: "a.example.org", Options: map[string]string{"StrictHostKeyChecking": "no"}},
		}},
		"a trust keyword in another case": {Roles: map[string]v1alpha1.HostRole{
			"a": {Host: "a.example.org", Options: map[string]string{"userknownhostsfile": "/dev/null"}},
		}},
		"a Match in a role option": {Roles: map[string]v1alpha1.HostRole{
			"a": {Host: "a.example.org", Options: map[string]string{"Match": "all"}},
		}},
		"a Host in a global option": {SSH: v1alpha1.SSHSpec{Options: map[string]string{"Host": "*"}}},
		"a keyword the generator writes, in a global option": {SSH: v1alpha1.SSHSpec{
			Options: map[string]string{"ConnectTimeout": "99"}}},
		"a key that is not one word": {SSH: v1alpha1.SSHSpec{Options: map[string]string{"Port=22 User": "x"}}},
		"a wildcard host": {Roles: map[string]v1alpha1.HostRole{
			"a": {Host: "*", ForwardAgent: true},
		}},
		"a misspelt jump role": {Roles: map[string]v1alpha1.HostRole{
			"mgmt": {Host: "mgmt.example.org"},
			"a":    {Host: "a.example.org", ProxyJump: "mgnt"},
		}},
	}
	for name, opts := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts.KnownHostsFile = "/k"
			if err := opts.Validate(); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}

	t.Run("roles sharing a host with the same settings are accepted", func(t *testing.T) {
		t.Parallel()
		opts := transport.Options{KnownHostsFile: "/k", Roles: map[string]v1alpha1.HostRole{
			"a": {Host: "gw.example.org", User: "root", ForwardAgent: true},
			"b": {Host: "gw.example.org", ForwardAgent: true},
		}}
		if err := opts.Validate(); err != nil {
			t.Error(err)
		}
	})
}

func TestLegacyKeyTypesReachEveryHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, err := transport.New(transport.Options{
		SSH:            v1alpha1.SSHSpec{LegacyKeyTypes: true},
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "k"),
	}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	// ssh -G prints the keyword under the name the client knows it by.
	got := resolved(t, path, "exe0001.example.org")
	if got := got["pubkeyacceptedalgorithms"] + got["pubkeyacceptedkeytypes"]; !strings.Contains(got, "ssh-rsa") {
		t.Errorf("a compute node does not accept ssh-rsa: %q", got)
	}
}

func TestDurationsAreNeverTruncatedToZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, err := transport.New(transport.Options{
		SSH: v1alpha1.SSHSpec{
			ConnectTimeout:      v1alpha1.Duration(500 * time.Millisecond),
			ServerAliveInterval: v1alpha1.Duration(1500 * time.Millisecond),
		},
		Roles:          map[string]v1alpha1.HostRole{"gw": {Host: "gw.example.org", ControlMaster: true}},
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "k"),
	}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	got := resolved(t, path, "gw.example.org")
	if got["connecttimeout"] != "1" {
		t.Errorf("a half second connect timeout became %q, want 1", got["connecttimeout"])
	}
	if got["serveraliveinterval"] != "2" {
		t.Errorf("a 1.5 second keepalive became %q, want 2", got["serveraliveinterval"])
	}
	// An explicit zero switches persistence off rather than keeping the
	// master for ever.
	if got["controlpersist"] != "no" {
		t.Errorf("controlPersist 0 became %q, want no", got["controlpersist"])
	}
}

func TestIncludeGlobsArePassedToSSH(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "conf.d", "one.conf"), "Host glob.example.org\n  Port 2200\n")
	path, err := transport.New(transport.Options{
		SSH:            v1alpha1.SSHSpec{Include: []string{filepath.Join(dir, "conf.d", "*.conf")}},
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "k"),
	}).ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved(t, path, "glob.example.org")["port"]; got != "2200" {
		t.Errorf("an include glob was dropped: port %q", got)
	}
}
