// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// TestArgsEndOptionsBeforeTheDestination covers the review's finding that a
// node name beginning with "-" became an ssh option: the destination has to
// come after "--", whatever it is, as it already does for scp.
func TestArgsEndOptionsBeforeTheDestination(t *testing.T) {
	t.Parallel()
	c := testClient(t)

	for _, req := range []transport.Request{{}, {Argv: []string{"uptime"}}} {
		args, err := c.Args(transport.Target{Name: "exe0001", Host: "exe0001.hpc.example.org"}, req)
		if err != nil {
			t.Fatalf("Args failed: %v", err)
		}
		end := slices.Index(args, "--")
		dest := slices.Index(args, "alice_adm@exe0001.hpc.example.org")
		if end < 0 || dest != end+1 {
			t.Errorf("Args = %q, want the destination straight after --", args)
		}
		if len(req.Argv) > 0 && !strings.HasSuffix(args[len(args)-1], " sh uptime") {
			t.Errorf("Args = %q, want the command last", args)
		}
	}
}

// notDestinations are targets whose host or account ssh and scp would read
// as something else: an option, another host, a port or a second argument.
var notDestinations = []transport.Target{
	{Name: "n", Host: "-oProxyCommand=touch${IFS}/tmp/pwned1;#"},
	{Name: "n", Host: "-v"},
	{Name: "n", Host: "exe0001 -v"},
	{Name: "n", Host: "x@exe0001"},
	{Name: "n", Host: "exe0001:22"},
	{Name: "n", Host: ""},
	{Name: "n", Host: "exe0001", User: "-oProxyCommand=x"},
	{Name: "n", Host: "exe0001", User: "a@b"},
	{Name: "n", Host: "exe0001", User: "a b"},
	{Name: "n", Host: "exe0001", User: "a\nb"},
	{Name: "n", Host: "exe0001", User: "x:y"},
}

func TestArgsRefuseADestinationThatIsNotAHost(t *testing.T) {
	t.Parallel()
	// No default user, so that the destination is the bare host name, which
	// is what config init writes when --user is not given.
	dir := t.TempDir()
	c := transport.New(transport.Options{StateDir: dir, KnownHostsFile: filepath.Join(dir, "known_hosts")})

	for _, target := range notDestinations {
		args, err := c.Args(target, transport.Request{Argv: []string{"uptime"}})
		if err == nil {
			t.Errorf("Args(%+v) = %q, want it refused", target, args)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("Args(%+v): exit code = %d, want %d", target, got, want)
		}
	}
}

// TestCopyArgsRefuseWhatArgsRefuse: scp was handed the account unchecked,
// so copy -u 'x:y' built x:y@exe0001:/tmp/, which scp reads as the host x,
// where a command with the same -u was refused.
func TestCopyArgsRefuseWhatArgsRefuse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := transport.New(transport.Options{StateDir: dir, KnownHostsFile: filepath.Join(dir, "known_hosts")})

	for _, target := range notDestinations {
		for _, upload := range []bool{true, false} {
			args, err := c.CopyArgs(target, transport.CopyRequest{
				Sources: []string{"/etc/hosts"}, Destination: "/tmp/", Upload: upload,
			})
			if err == nil {
				t.Errorf("CopyArgs(%+v, upload %v) = %q, want it refused", target, upload, args)
				continue
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("CopyArgs(%+v, upload %v): exit code = %d, want %d", target, upload, got, want)
			}
		}
	}
}

func TestArgsAcceptAddressesAndAccounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := transport.New(transport.Options{
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "known_hosts"),
		Roles:          map[string]v1alpha1.HostRole{"dhcp": {Host: "dhcp01.example.org", User: "root"}},
	})

	for _, target := range []transport.Target{
		{Name: "dhcp", Host: "dhcp01.example.org", Role: "dhcp"},
		{Name: "n", Host: "10.0.1.1"},
		{Name: "n", Host: "fe80::1"},
		{Name: "n", Host: "wlm01.hpc.example.org."},
		{Name: "n", Host: "exe0001", User: "alice_adm"},
		{Name: "n", Host: "exe0001", User: "svc.backup-1"},
	} {
		if _, err := c.Args(target, transport.Request{}); err != nil {
			t.Errorf("Args(%+v) = %v, want it accepted", target, err)
		}
	}
}
