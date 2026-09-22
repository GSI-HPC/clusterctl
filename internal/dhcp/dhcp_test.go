// SPDX-License-Identifier: LGPL-3.0-or-later

package dhcp_test

import (
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/dhcp"
)

const sample = `
# managed by configuration management
option domain-name "hpc.example.org";

subnet 10.0.0.0 netmask 255.255.0.0 {
  range 10.0.99.1 10.0.99.254;
}

# exe0001 first interface
host exe0001 {
  hardware ethernet AA:BB:CC:11:22:33;
  fixed-address 10.0.1.1;
  filename "/srv/pxesrv/boot/exe/ipxe.net2";
  option dhcp-client-identifier "ff:00:00:01";
  option host-name "exe0001";
}

host exe0001-ib {
  # the second interface has the options in another order
  option dhcp-client-identifier "ff:00:00:02";
  filename "/srv/pxesrv/boot/exe/ipxe.ib0";
  fixed-address 10.1.1.1;
  hardware ethernet aa:bb:cc:11:22:44;
}

# a node named only in the comment: sub0001
host weird-name-0001 {
  hardware ethernet AA:BB:CC:99:99:99;
  fixed-address 10.0.5.1;
}
`

func parse(t *testing.T) *dhcp.Config {
	t.Helper()
	cfg, err := dhcp.Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	return cfg
}

func TestParseReadsHostDeclarations(t *testing.T) {
	t.Parallel()
	cfg := parse(t)

	if got, want := len(cfg.Hosts), 3; got != want {
		t.Fatalf("got %d hosts, want %d", got, want)
	}
	byName := map[string]dhcp.Host{}
	for _, h := range cfg.Hosts {
		byName[h.Name] = h
	}

	first := byName["exe0001"]
	if got, want := first.Address, "10.0.1.1"; got != want {
		t.Errorf("address = %q, want %q", got, want)
	}
	if got, want := strings.Join(first.MACs, ","), "aa:bb:cc:11:22:33"; got != want {
		t.Errorf("macs = %q, want %q", got, want)
	}
	if got, want := first.Filename, "/srv/pxesrv/boot/exe/ipxe.net2"; got != want {
		t.Errorf("filename = %q, want %q", got, want)
	}
	if got, want := first.ClientIdentifier, "ff:00:00:01"; got != want {
		t.Errorf("client identifier = %q, want %q", got, want)
	}
	if got, want := first.Options["host-name"], "exe0001"; got != want {
		t.Errorf("option host-name = %q, want %q", got, want)
	}

	// Reading a fixed number of lines after a match, which the shell tools
	// did, gets this declaration wrong because its options are in another
	// order.
	second := byName["exe0001-ib"]
	if got, want := second.Address, "10.1.1.1"; got != want {
		t.Errorf("second interface address = %q, want %q", got, want)
	}
	if got, want := second.Filename, "/srv/pxesrv/boot/exe/ipxe.ib0"; got != want {
		t.Errorf("second interface filename = %q, want %q", got, want)
	}
}

func TestLookupFindsEveryInterfaceOfANode(t *testing.T) {
	t.Parallel()
	cfg := parse(t)

	hosts := cfg.Lookup("exe0001")
	if got, want := len(hosts), 2; got != want {
		t.Fatalf("got %d declarations for exe0001, want %d", got, want)
	}

	// A site that names the node only in a comment is still matched.
	if got := cfg.Lookup("sub0001"); len(got) != 1 {
		t.Errorf("got %d declarations for a node named in a comment, want 1", len(got))
	}
	if got := cfg.Lookup("exe0002"); len(got) != 0 {
		t.Errorf("got %d declarations for an unknown node, want 0", len(got))
	}
}

func TestParseReportsAnUnclosedDeclaration(t *testing.T) {
	t.Parallel()

	_, err := dhcp.Parse([]byte("host exe1 {\n  fixed-address 10.0.0.1;\n"))
	if err == nil {
		t.Fatal("an unclosed declaration should be reported")
	}
	if !strings.Contains(err.Error(), "exe1") {
		t.Errorf("error = %v, want it to name the declaration", err)
	}
}

func TestGUIDFromMAC(t *testing.T) {
	t.Parallel()

	got, err := dhcp.GUIDFromMAC("AA:BB:CC:11:22:33")
	if err != nil {
		t.Fatalf("GUIDFromMAC failed: %v", err)
	}
	if want := "0xaabbcc03001122 33"; got != strings.ReplaceAll(want, " ", "") {
		t.Errorf("GUIDFromMAC = %q, want %q", got, strings.ReplaceAll(want, " ", ""))
	}

	for _, bad := range []string{"", "aa:bb", "zz:bb:cc:11:22:33"} {
		if _, err := dhcp.GUIDFromMAC(bad); err == nil {
			t.Errorf("GUIDFromMAC(%q) should fail", bad)
		}
	}
}
