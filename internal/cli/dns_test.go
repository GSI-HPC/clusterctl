// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS serves a fixed zone over UDP on the loopback address and returns
// the address to point services.dns.server at. A name it has no records for
// is answered with NXDOMAIN.
func fakeDNS(t *testing.T, zone map[string][]dnsmessage.Resource) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(buf[:n]); err != nil || len(query.Questions) != 1 {
				continue
			}
			q := query.Questions[0]
			resp := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true},
				Questions: query.Questions,
			}
			// Follow the aliases the way a recursive server does, and put
			// every record on the way into the answer.
			name, found := strings.ToLower(q.Name.String()), false
			for range maxCNAMEs {
				records, ok := zone[name]
				if !ok {
					break
				}
				found = true
				next := ""
				for _, rr := range records {
					switch body := rr.Body.(type) {
					case *dnsmessage.CNAMEResource:
						resp.Answers = append(resp.Answers, rr)
						next = body.CNAME.String()
					default:
						if rr.Header.Type == q.Type {
							resp.Answers = append(resp.Answers, rr)
						}
					}
				}
				if next == "" {
					break
				}
				name = next
			}
			if !found {
				resp.RCode = dnsmessage.RCodeNameError
			}
			packed, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(packed, from)
		}
	}()
	return conn.LocalAddr().String()
}

func rrHeader(name string, qtype dnsmessage.Type) dnsmessage.ResourceHeader {
	return dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: qtype, Class: dnsmessage.ClassINET, TTL: 60}
}

func cnameRR(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypeCNAME),
		Body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)}}
}

func aRR(name string, ip string) dnsmessage.Resource {
	var a [4]byte
	copy(a[:], net.ParseIP(ip).To4())
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypeA), Body: &dnsmessage.AResource{A: a}}
}

func ptrRR(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypePTR),
		Body: &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(target)}}
}

// TestDNSLookupShowsTheChainOfAliases is the report's 12.12: the help
// promised that a chain of aliases is shown as it is, and LookupHost returns
// addresses only.
func TestDNSLookupShowsTheChainOfAliases(t *testing.T) {
	server := fakeDNS(t, map[string][]dnsmessage.Resource{
		"exe0001.hpc.example.org.":    {cnameRR("exe0001.hpc.example.org.", "exe0001-ib.hpc.example.org.")},
		"exe0001-ib.hpc.example.org.": {cnameRR("exe0001-ib.hpc.example.org.", "n1.ib.example.org.")},
		"n1.ib.example.org.":          {aRR("n1.ib.example.org.", "10.0.2.1")},
	})
	h, err := run(t, harnessOptions{}, "dns", "lookup", "-n", "exe0001", "-o", "json",
		"--set", "services.dns.server="+server)
	if err != nil {
		t.Fatalf("dns lookup failed: %v\n%s", err, h.out)
	}
	var got map[string]dnsAnswer
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.out)
	}
	answer := got["exe0001.hpc.example.org"]
	if strings.Join(answer.CNAMEs, " ") != "exe0001-ib.hpc.example.org n1.ib.example.org" {
		t.Errorf("chain = %v, want both aliases in order", answer.CNAMEs)
	}
	if strings.Join(answer.Addresses, " ") != "10.0.2.1" {
		t.Errorf("addresses = %v, want 10.0.2.1", answer.Addresses)
	}
}

// TestDNSAliasesAskTheServerAndNotTheHostsFile is the report's 12.12: the Go
// resolver answered from /etc/hosts before it asked the configured server,
// and only the first name of a reverse entry was shown.
func TestDNSAliasesAskTheServerAndNotTheHostsFile(t *testing.T) {
	server := fakeDNS(t, map[string][]dnsmessage.Resource{
		"submit.hpc.example.org.": {cnameRR("submit.hpc.example.org.", "pool.hpc.example.org.")},
		"pool.hpc.example.org.": {
			aRR("pool.hpc.example.org.", "10.0.3.1"),
			aRR("pool.hpc.example.org.", "10.0.3.2"),
		},
		"1.3.0.10.in-addr.arpa.": {
			ptrRR("1.3.0.10.in-addr.arpa.", "sub0001.hpc.example.org."),
			ptrRR("1.3.0.10.in-addr.arpa.", "submit-a.hpc.example.org."),
		},
		"2.3.0.10.in-addr.arpa.": {ptrRR("2.3.0.10.in-addr.arpa.", "sub0002.hpc.example.org.")},
	})
	h, err := run(t, harnessOptions{}, "dns", "aliases", "-o", "json",
		"--set", "services.dns.server="+server,
		"--set", `services.dns.aliases=["submit", "localhost."]`)
	// localhost is in every /etc/hosts and not in this server.
	if err == nil {
		t.Errorf("localhost resolved although the server does not know it:\n%s", h.out)
	}
	if strings.Contains(h.out.String(), "127.0.0.1") {
		t.Errorf("an answer came from /etc/hosts:\n%s", h.out)
	}
	var got map[string]aliasAnswer
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.out)
	}
	answer := got["submit.hpc.example.org"]
	if strings.Join(answer.CNAMEs, " ") != "pool.hpc.example.org" {
		t.Errorf("chain = %v, want pool.hpc.example.org", answer.CNAMEs)
	}
	if len(answer.Addresses) != 2 {
		t.Fatalf("addresses = %+v, want two", answer.Addresses)
	}
	if got := strings.Join(answer.Addresses[0].Hosts, " "); got != "sub0001.hpc.example.org submit-a.hpc.example.org" {
		t.Errorf("hosts of %s = %q, want every name of the reverse entry", answer.Addresses[0].Address, got)
	}
}

// TestDNSServerTakesAnIPv6Address is the report's 12.12: an IPv6 server
// without brackets or port failed every lookup with "too many colons".
func TestDNSServerTakesAnIPv6Address(t *testing.T) {
	h, _ := run(t, harnessOptions{}, "dns", "lookup", "-n", "exe0001",
		"--set", "services.dns.server=::1", "--set", "services.dns.timeout=200ms")
	if strings.Contains(h.out.String()+h.errOut.String(), "too many colons") {
		t.Errorf("the IPv6 server was not understood:\n%s%s", h.out, h.errOut)
	}

	for server, want := range map[string]string{
		"fd00::53":        "[fd00::53]:53",
		"[fd00::53]":      "[fd00::53]:53",
		"[fd00::53]:5353": "[fd00::53]:5353",
		"10.0.0.1":        "10.0.0.1:53",
		"10.0.0.1:5353":   "10.0.0.1:5353",
		"ns.example.org":  "ns.example.org:53",
	} {
		got, err := dnsServerAddress(server)
		if err != nil || got != want {
			t.Errorf("dnsServerAddress(%q) = %q, %v; want %q", server, got, err, want)
		}
	}
	for _, server := range []string{"[]", ":53", "10.0.0.1:"} {
		if got, err := dnsServerAddress(server); err == nil {
			t.Errorf("dnsServerAddress(%q) = %q, want an error", server, got)
		}
	}
}

func TestReverseNames(t *testing.T) {
	for address, want := range map[string]string{
		"10.0.3.1":    "1.3.0.10.in-addr.arpa.",
		"2001:db8::1": "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.",
	} {
		if got, err := reverseName(address); err != nil || got != want {
			t.Errorf("reverseName(%q) = %q, %v; want %q", address, got, err, want)
		}
	}
}

// TestDNSLookupBMCTakesTheInventoryAddress: lookup --bmc resolved the name
// the naming rules derive, which may be stale or missing, instead of the
// bmcAddress the bmc commands reach. An address is shown as it is, and
// marked as not looked up, since no server was asked about it.
func TestDNSLookupBMCTakesTheInventoryAddress(t *testing.T) {
	server := fakeDNS(t, map[string][]dnsmessage.Resource{
		"exe0004.mgmt.hpc.example.org.": {aRR("exe0004.mgmt.hpc.example.org.", "10.9.0.4")},
	})
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0003\n      bmcAddress: 10.9.0.77\n"
	})
	h, err := run(t, harnessOptions{config: []string{inventory}}, "dns", "lookup", "--bmc",
		"-n", "exe[0003-0004]", "-o", "json", "--set", "services.dns.server="+server)
	if err != nil {
		t.Fatalf("dns lookup --bmc failed: %v\n%s%s", err, h.out, h.errOut)
	}
	var got map[string]dnsAnswer
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.out)
	}
	if answer, ok := got["10.9.0.77"]; !ok || strings.Join(answer.Addresses, " ") != "10.9.0.77" || !answer.Literal {
		t.Errorf("answers = %+v, want exe0003's service processor shown as 10.9.0.77, not looked up", got)
	}
	if answer := got["exe0004.mgmt.hpc.example.org"]; strings.Join(answer.Addresses, " ") != "10.9.0.4" || answer.Literal {
		t.Errorf("answers = %+v, want exe0004's service processor resolved by its derived name", got)
	}
	if !strings.Contains(h.errOut.String(), "10.9.0.77") {
		t.Errorf("no note says 10.9.0.77 was not looked up:\n%s", h.errOut)
	}
}
