// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package dhcp_test

import (
	"errors"
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

// Layouts the line-based parser merged or dropped (report 5.2).
func TestParseReadsEveryLayoutDhcpdAccepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		conf string
		want map[string]dhcp.Host
	}{
		{
			name: "a brace that shares its line with the last statement",
			conf: "host a {\n  hardware ethernet aa:00:00:00:00:01;\n  fixed-address 10.0.0.1; }\n" +
				"host b {\n  hardware ethernet aa:00:00:00:00:02;\n  fixed-address 10.0.0.2;\n}\n",
			want: map[string]dhcp.Host{
				"a": {Address: "10.0.0.1", MACs: []string{"aa:00:00:00:00:01"}},
				"b": {Address: "10.0.0.2", MACs: []string{"aa:00:00:00:00:02"}},
			},
		},
		{
			name: "trailing braces throughout, inside a group",
			conf: "group {\n  host a {\n    fixed-address 10.0.0.1; }\n  host b {\n    fixed-address 10.0.0.2; }\n}\n",
			want: map[string]dhcp.Host{"a": {Address: "10.0.0.1"}, "b": {Address: "10.0.0.2"}},
		},
		{
			name: "a declaration on one line",
			conf: "host a { hardware ethernet AA:00:00:00:00:01; fixed-address 10.0.0.1; }\n",
			want: map[string]dhcp.Host{"a": {Address: "10.0.0.1", MACs: []string{"aa:00:00:00:00:01"}}},
		},
		{
			name: "statements after the brace on the host line",
			conf: "host a { fixed-address 10.0.0.1;\n  hardware ethernet aa:00:00:00:00:01;\n}\n",
			want: map[string]dhcp.Host{"a": {Address: "10.0.0.1", MACs: []string{"aa:00:00:00:00:01"}}},
		},
		{
			name: "two statements on one line",
			conf: "host a {\n  hardware ethernet aa:00:00:00:00:01; fixed-address 10.0.0.1;\n}\n",
			want: map[string]dhcp.Host{"a": {Address: "10.0.0.1", MACs: []string{"aa:00:00:00:00:01"}}},
		},
		{
			name: "a # inside a quoted string",
			conf: "host a {\n  filename \"/boot/#1/ipxe\"; # the first\n  fixed-address 10.0.0.1;\n}\n",
			want: map[string]dhcp.Host{"a": {Address: "10.0.0.1", Filename: "/boot/#1/ipxe"}},
		},
		{
			name: "a quoted host name and several addresses",
			conf: "host \"a\" {\n  fixed-address 10.0.0.1, 10.0.0.2;\n}\n",
			want: map[string]dhcp.Host{"a": {Address: "10.0.0.1, 10.0.0.2"}},
		},
		{
			name: "an option definition with a record type",
			conf: "option foo code 224 = { unsigned integer 8, text };\n" +
				"subnet 10.0.0.0 netmask 255.0.0.0 {\n  pool { range 10.0.99.1 10.0.99.9; }\n" +
				"  host a { fixed-address 10.0.0.1; }\n}\n",
			want: map[string]dhcp.Host{"a": {Address: "10.0.0.1"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := dhcp.Parse([]byte(tc.conf))
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			if got, want := len(cfg.Hosts), len(tc.want); got != want {
				t.Fatalf("got %d hosts, want %d: %+v", got, want, cfg.Hosts)
			}
			for _, h := range cfg.Hosts {
				want, ok := tc.want[h.Name]
				if !ok {
					t.Errorf("unexpected declaration %q", h.Name)
					continue
				}
				if h.Address != want.Address {
					t.Errorf("%s: address = %q, want %q", h.Name, h.Address, want.Address)
				}
				if got, want := strings.Join(h.MACs, ","), strings.Join(want.MACs, ","); got != want {
					t.Errorf("%s: macs = %q, want %q", h.Name, got, want)
				}
				if h.Filename != want.Filename {
					t.Errorf("%s: filename = %q, want %q", h.Name, h.Filename, want.Filename)
				}
			}
		})
	}
}

// What the parser does not understand stops it, rather than being merged into
// a neighbouring declaration (report 5.2).
func TestParseRefusesWhatItDoesNotUnderstand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, conf, want string
	}{
		{"a host inside a host", "host a {\n  fixed-address 10.0.0.1;\nhost b {\n}\n}\n", `"b"`},
		{"a host statement inside a host", "host a {\n  host b;\n}\n", `"b"`},
		{"a block inside a host", "host a {\n  if exists foo { fixed-address 10.0.0.9; }\n}\n", "unexpected block"},
		{"a statement without a semicolon", "host a {\n  fixed-address 10.0.0.1\n}\n", "not ended with ;"},
		{"a second fixed-address", "host a { fixed-address 10.0.0.1; fixed-address 10.0.0.2; }\n", "more than one fixed-address"},
		{"a stray closing brace", "}\n", "without a matching"},
		{"an unclosed block", "subnet 10.0.0.0 netmask 255.0.0.0 {\n", "not closed"},
		{"an unclosed string", "host a { filename \"x; }\n", "not closed"},
		{"a host with two names", "host a b { }\n", "exactly one name"},
		{"a host without a body", "host a;\n", "no body"},
		{"an include without a reader", "include \"/etc/dhcp/hosts.conf\";\n", "cannot be followed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := dhcp.Parse([]byte(tc.conf))
			if err == nil {
				t.Fatal("Parse should fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseFileFollowsInclude(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"/etc/dhcp/dhcpd.conf": "include \"/etc/dhcp/a.conf\";\nhost x { fixed-address 10.0.0.9; }\n",
		"/etc/dhcp/a.conf":     "group {\n  include \"/etc/dhcp/b.conf\";\n}\n",
		"/etc/dhcp/b.conf":     "host y { fixed-address 10.0.0.8; }\n",
		"/etc/dhcp/loop.conf":  "include \"/etc/dhcp/loop.conf\";\n",
		"/etc/dhcp/rel.conf":   "include \"hosts.conf\";\n",
		"/etc/dhcp/gone.conf":  "include \"/etc/dhcp/missing.conf\";\n",
	}
	missing := errors.New("no such file")
	read := func(file string) ([]byte, error) {
		content, ok := files[file]
		if !ok {
			return nil, missing
		}
		return []byte(content), nil
	}

	cfg, err := dhcp.ParseFile("/etc/dhcp/dhcpd.conf", read)
	if err != nil {
		t.Fatalf("ParseFile failed: %v", err)
	}
	if got, want := len(cfg.Hosts), 2; got != want {
		t.Fatalf("got %d hosts, want %d", got, want)
	}

	for file, want := range map[string]string{
		"/etc/dhcp/loop.conf": "includes itself",
		"/etc/dhcp/rel.conf":  "relative",
		"/etc/dhcp/gone.conf": "/etc/dhcp/gone.conf:1",
	} {
		_, err := dhcp.ParseFile(file, read)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseFile(%s) = %v, want an error containing %q", file, err, want)
		}
	}
	// The reader's own error survives, so its exit code does too.
	if _, err := dhcp.ParseFile("/etc/dhcp/gone.conf", read); !errors.Is(err, missing) {
		t.Errorf("ParseFile = %v, want it to wrap the reader's error", err)
	}
}
