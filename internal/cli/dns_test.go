// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
)

// fakeDNS serves a fixed zone over UDP on the loopback address and returns
// the address to point services.dns.server at. A name it has no records for
// is answered with NXDOMAIN.
func fakeDNS(t *testing.T, zone map[string][]dnsmessage.Resource) string {
	t.Helper()
	return (&dnsServer{zone: zone}).serve(t)
}

// dnsServer is a fake name server over a fixed zone.
type dnsServer struct {
	zone map[string][]dnsmessage.Resource
	// inFlight, when it is set, counts the queries under way, each
	// answered on a goroutine of its own, as a server asked about several
	// names at once answers them. A query stops counting before its answer
	// is sent: the client's next question can arrive as soon as the answer
	// does, and counted after it, one query would be counted twice.
	inFlight *fanouttest.InFlight
	// delay, when it is set, holds back the answers about a name, which
	// it is given in lower case with its final dot.
	delay func(name string) time.Duration
	// silent are names, in lower case with their final dot, that are
	// never answered.
	silent map[string]bool
	// failing are names, in lower case with their final dot, that are
	// answered with the response code given and no records, as a server
	// answers when it cannot say.
	failing map[string]dnsmessage.RCode
	// asked counts the queries that arrived, and onQuery, when it is set,
	// is told the count as each arrives.
	asked   atomic.Int32
	onQuery func(asked int32)
}

// serve answers queries over UDP on the loopback address until the test
// ends, and returns the address to point services.dns.server at.
func (s *dnsServer) serve(t *testing.T) string {
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
			if asked := s.asked.Add(1); s.onQuery != nil {
				s.onQuery(asked)
			}
			var query dnsmessage.Message
			if err := query.Unpack(buf[:n]); err != nil || len(query.Questions) != 1 {
				continue
			}
			if s.silent[strings.ToLower(query.Questions[0].Name.String())] {
				continue
			}
			if s.inFlight == nil && s.delay == nil {
				s.answer(conn, from, query)
				continue
			}
			go func() {
				leave := func() {}
				if s.inFlight != nil {
					leave = s.inFlight.Enter()
				}
				if s.delay != nil {
					time.Sleep(s.delay(strings.ToLower(query.Questions[0].Name.String())))
				}
				leave()
				s.answer(conn, from, query)
			}()
		}
	}()
	return conn.LocalAddr().String()
}

// answer sends the answer to one query.
func (s *dnsServer) answer(conn net.PacketConn, from net.Addr, query dnsmessage.Message) {
	q := query.Questions[0]
	resp := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true},
		Questions: query.Questions,
	}
	if rcode, ok := s.failing[strings.ToLower(q.Name.String())]; ok {
		resp.RCode = rcode
		if packed, err := resp.Pack(); err == nil {
			_, _ = conn.WriteTo(packed, from)
		}
		return
	}
	// Follow the aliases the way a recursive server does, and put every
	// record on the way into the answer.
	name, found := strings.ToLower(q.Name.String()), false
	for range maxCNAMEs {
		records, ok := s.zone[name]
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
		return
	}
	_, _ = conn.WriteTo(packed, from)
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
	t.Parallel()
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
	t.Parallel()
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

// A reverse lookup that failed, because the server answered SERVFAIL or
// did not answer in time, was shown as "hosts": [] in -o json and exited 0,
// which is what an address without a reverse entry looks like: a pool whose
// server could not say which machines are in it read as an empty pool. An
// address without a reverse entry is still not a failure.
func TestDNSAliasesFailWhenAReverseLookupFails(t *testing.T) {
	t.Parallel()
	const failing = "2.3.0.10.in-addr.arpa."
	for _, tc := range []struct {
		name   string
		server *dnsServer
		code   int
		error  string
	}{
		{"SERVFAIL", &dnsServer{failing: map[string]dnsmessage.RCode{failing: dnsmessage.RCodeServerFailure}},
			exitcode.TargetFailed, "answered ServerFailure"},
		{"no answer", &dnsServer{silent: map[string]bool{failing: true}},
			exitcode.TargetFailed, "did not answer within 200ms"},
		{"no reverse entry", &dnsServer{}, exitcode.OK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.server.zone = map[string][]dnsmessage.Resource{
				"submit.hpc.example.org.": {
					aRR("submit.hpc.example.org.", "10.0.3.1"),
					aRR("submit.hpc.example.org.", "10.0.3.2"),
				},
				"1.3.0.10.in-addr.arpa.": {ptrRR("1.3.0.10.in-addr.arpa.", "sub0001.hpc.example.org.")},
			}
			server := tc.server.serve(t)
			h, err := run(t, harnessOptions{}, "dns", "aliases", "-o", "json",
				"--set", "services.dns.server="+server, "--set", "services.dns.timeout=200ms",
				"--set", `services.dns.aliases=["submit"]`)
			wantCode(t, err, tc.code)
			var got map[string]aliasAnswer
			if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, h.out)
			}
			addresses := got["submit.hpc.example.org"].Addresses
			if len(addresses) != 2 || strings.Join(addresses[0].Hosts, " ") != "sub0001.hpc.example.org" ||
				addresses[0].Error != "" {
				t.Fatalf("addresses = %+v, want 10.0.3.1 with its host first", addresses)
			}
			if second := addresses[1]; len(second.Hosts) != 0 || second.Hosts == nil ||
				!strings.Contains(second.Error, tc.error) || (tc.error == "") != (second.Error == "") {
				t.Errorf("10.0.3.2 = %+v, want no host and the error %q", second, tc.error)
			}
		})
	}
}

// TestDNSServerTakesAnIPv6Address is the report's 12.12: an IPv6 server
// without brackets or port failed every lookup with "too many colons".
func TestDNSServerTakesAnIPv6Address(t *testing.T) {
	t.Parallel()
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

// The system name servers are the nameserver lines of resolv.conf, each
// with port 53, an IPv6 address in brackets. Comments, other keywords and a
// nameserver line without an address name none.
func TestResolvConfServersReadsTheNameserverLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string
		want    []string
	}{
		{"one server", "nameserver 10.0.0.53\n", []string{"10.0.0.53:53"}},
		{"every line in order", `# written by NetworkManager
; nameserver 192.0.2.1
search hpc.example.org example.org
nameserver 10.0.0.53
  nameserver   10.0.0.54   # the second
	nameserver fd00::53
nameserver fe80::1%eth0
nameserver
options ndots:2 timeout:1
`, []string{"10.0.0.53:53", "10.0.0.54:53", "[fd00::53]:53", "[fe80::1%eth0]:53"}},
		{"none", "# nothing\nsearch example.org\n", nil},
		{"an empty file", "", nil},
		{"no final newline", "nameserver 10.0.0.53", []string{"10.0.0.53:53"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := resolvConfServers(path)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("resolvConfServers = %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	if got, err := resolvConfServers(filepath.Join(t.TempDir(), "resolv.conf")); err == nil {
		t.Errorf("a file that is not there gave %q and no error", got)
	}
}

func TestReverseNames(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// dns lookup asked one name at a time, so a node set of dead names took the
// timeout once per name. services.dns.maxConcurrent bounds the names asked
// at once, and --fanout lowers it where it is lower but never raises it,
// while fanout.max from --set or the environment leaves it alone. Each query
// is held until one more than the limit are under way, which never happens
// while the limit is kept, so exactly the limit run at once; a name is
// asked its two questions one after the other, so the queries under way are
// the names under way.
func TestDNSLookupKeepsToItsLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want int
	}{
		{"services.dns.maxConcurrent alone", "", nil, 3},
		{"a lower --fanout", "", []string{"--fanout", "2"}, 2},
		{"a higher --fanout", "", []string{"--fanout", "8"}, 3},
		{"fanout.max with --set", "", []string{"--set", "fanout.max=1"}, 3},
		{"fanout.max from the environment", "1", nil, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("CLUSTERCTL_FANOUT", tc.env)
			}
			s := &dnsServer{inFlight: &fanouttest.InFlight{Hold: tc.want + 1}}
			server := s.serve(t)
			args := append([]string{"--set", "services.dns.server=" + server, "--set", "services.dns.maxConcurrent=3"}, tc.args...)
			h, err := run(t, harnessOptions{}, append(args, "dns", "lookup", "-n", "exe[0001-0005]")...)
			wantCode(t, err, exitcode.TargetFailed)
			if got := s.inFlight.Peak(); got != tc.want {
				t.Errorf("%d queries were under way at once, want %d\n%s", got, tc.want, h.out)
			}
			if got := s.inFlight.Started(); got != 10 {
				t.Errorf("%d queries were asked, want the two questions of each of the 5 names", got)
			}
		})
	}
}

// The names are listed in the order of the nodes, whatever order their
// answers came in, and a failure and an address that was not looked up keep
// their place among them.
func TestDNSLookupListsTheNodesInTheirOrder(t *testing.T) {
	t.Parallel()
	s := &dnsServer{
		zone: map[string][]dnsmessage.Resource{
			"exe0001.mgmt.hpc.example.org.": {aRR("exe0001.mgmt.hpc.example.org.", "10.9.0.1")},
			"exe0004.mgmt.hpc.example.org.": {aRR("exe0004.mgmt.hpc.example.org.", "10.9.0.4")},
		},
		// The first name is answered last.
		delay: func(name string) time.Duration {
			if strings.HasPrefix(name, "exe0001.") {
				return 150 * time.Millisecond
			}
			return 0
		},
	}
	server := s.serve(t)
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0003\n      bmcAddress: 10.9.0.77\n"
	})
	h, err := run(t, harnessOptions{config: []string{inventory}}, "dns", "lookup", "--bmc",
		"-n", "exe[0001-0004]", "--set", "services.dns.server="+server)
	wantCode(t, err, exitcode.TargetFailed)
	if err == nil || err.Error() != "1 of 4 names did not resolve" {
		t.Errorf("error = %v, want 1 of 4 names did not resolve", err)
	}
	want := `
NODE     HOST                          CNAME  ADDRESSES
exe0001  exe0001.mgmt.hpc.example.org         10.9.0.1
exe0002  exe0002.mgmt.hpc.example.org         no answer: no such host
exe0003  10.9.0.77                            10.9.0.77
exe0004  exe0004.mgmt.hpc.example.org         10.9.0.4
`
	if got := h.out.String(); got != want[1:] {
		t.Errorf("output:\n%s\nwant:\n%s", got, want[1:])
	}
	if got, want := h.errOut.String(), "exe0003: 10.9.0.77 is an address, not a name; it was not looked up\n"; got != want {
		t.Errorf("standard error = %q, want only the note about exe0003", got)
	}
}

// Every node's host is worked out before a name is looked up, so a node the
// naming rules cannot name stops the command before any server is asked.
func TestDNSLookupNamesEveryNodeBeforeItAsks(t *testing.T) {
	t.Parallel()
	s := &dnsServer{}
	server := s.serve(t)
	_, err := run(t, harnessOptions{}, "dns", "lookup", "--bmc", "-n", "exe0001,foo1",
		"--set", "services.dns.server="+server,
		"--set", `naming.rules=[{"match":{"prefixes":["exe"]},"fqdn":"{name}.hpc.example.org","bmc":"{name}.mgmt.example.org"},{"fqdn":"{name}.example.org"}]`)
	wantCode(t, err, exitcode.Usage)
	if err == nil || !strings.Contains(err.Error(), "foo1") {
		t.Errorf("error = %v, want foo1 named", err)
	}
	if got := s.asked.Load(); got != 0 {
		t.Errorf("%d queries were sent before the command stopped, want none", got)
	}
}

// Each name is a target of its own, and each question asked about it a
// call to the server: a name the server does not know is an answer, and
// one it never answers runs out of time.
func TestDNSLookupReportsEachName(t *testing.T) {
	t.Parallel()
	s := &dnsServer{
		zone:   map[string][]dnsmessage.Resource{"exe0001.hpc.example.org.": {aRR("exe0001.hpc.example.org.", "10.0.2.1")}},
		silent: map[string]bool{"exe0003.hpc.example.org.": true},
	}
	server := s.serve(t)
	ctx, tree := progresstest.Watch(context.Background(), t, byExitCode)
	_, err := run(t, harnessOptions{ctx: ctx}, "dns", "lookup", "-n", "exe[0001-0003]",
		"--set", "services.dns.server="+server, "--set", "services.dns.timeout=200ms")
	wantCode(t, err, exitcode.TargetFailed)
	want := `
command dns lookup: failed (target): 2 of 3 names did not resolve
  step resolve the names total=3 limit=16 [fold]: failed (target): 2 of 3 failed: exe[0002-0003]
    target exe0001: ok
      call dns host=SERVER timeout=200ms message=A {}: ok
      call dns host=SERVER timeout=200ms message=AAAA {}: ok
    target exe0002: failed (target): no such host
      call dns host=SERVER timeout=200ms message=A {}: ok
      call dns host=SERVER timeout=200ms message=AAAA {}: ok
    target exe0003: failed (timeout): SERVER did not answer within 200ms
      call dns host=SERVER timeout=200ms message=A {}: failed (timeout): SERVER did not answer within 200ms
`
	if got := strings.ReplaceAll(tree(), server, "SERVER"); got != want[1:] {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want[1:])
	}
}

// An interrupt ends the lookups under way and starts no other, and the
// command stops with it rather than print a table of names it never asked
// about. The queries it cut short say they were interrupted, not that the
// server did not answer in time.
func TestDNSLookupStopsWhenInterrupted(t *testing.T) {
	s := &dnsServer{silent: map[string]bool{}}
	for i := 1; i <= 5; i++ {
		s.silent[fmt.Sprintf("exe%04d.hpc.example.org.", i)] = true
	}
	watched, tree := progresstest.Watch(context.Background(), t, byExitCode)
	ctx, cancel := context.WithCancel(watched)
	defer cancel()
	// The interrupt comes once both names under way have been asked.
	s.onQuery = func(asked int32) {
		if asked == 2 {
			cancel()
		}
	}
	server := s.serve(t)
	start := time.Now()
	h, err := run(t, harnessOptions{ctx: ctx}, "--fanout", "2", "dns", "lookup", "-n", "exe[0001-0005]",
		"--set", "services.dns.server="+server, "--set", "services.dns.timeout=1m")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the interrupt", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("dns lookup took %v to stop after the interrupt", elapsed)
	}
	if h.out.Len() != 0 {
		t.Errorf("an interrupted lookup printed:\n%s", h.out)
	}
	if got := s.asked.Load(); got != 2 {
		t.Errorf("%d queries were sent, want only those of the two names under way when the interrupt came", got)
	}
	if got := tree(); !strings.Contains(got, "was interrupted before it was answered") || strings.Contains(got, "did not answer within") {
		t.Errorf("progress:\n%s\nwant the queries cut short to say they were interrupted", got)
	}
}

// dns aliases read the reverse entry of each address behind an alias one
// after the other, and again for every alias that led to the same pool.
// The addresses are read side by side, each once: the server holds each
// reverse query a while and counts those it holds at once.
func TestDNSAliasesReadTheReverseEntriesSideBySide(t *testing.T) {
	t.Parallel()
	var (
		mu        sync.Mutex
		held      int
		most      int
		reverse   = map[string]int{}
		addresses = []string{"10.0.3.1", "10.0.3.2", "10.0.3.3", "10.0.3.4"}
	)
	zone := map[string][]dnsmessage.Resource{
		"submit.hpc.example.org.": {cnameRR("submit.hpc.example.org.", "pool.hpc.example.org.")},
		"login.hpc.example.org.":  {cnameRR("login.hpc.example.org.", "pool.hpc.example.org.")},
	}
	for i, address := range addresses {
		zone["pool.hpc.example.org."] = append(zone["pool.hpc.example.org."], aRR("pool.hpc.example.org.", address))
		ptr := fmt.Sprintf("%d.3.0.10.in-addr.arpa.", i+1)
		zone[ptr] = []dnsmessage.Resource{ptrRR(ptr, fmt.Sprintf("sub%04d.hpc.example.org.", i+1))}
	}
	server := (&dnsServer{zone: zone, delay: func(name string) time.Duration {
		if !strings.HasSuffix(name, ".in-addr.arpa.") {
			return 0
		}
		mu.Lock()
		held++
		most = max(most, held)
		reverse[name]++
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		held--
		mu.Unlock()
		return 0
	}}).serve(t)
	h, err := run(t, harnessOptions{}, "dns", "aliases", "-o", "json",
		"--set", "services.dns.server="+server, "--set", `services.dns.aliases=["submit", "login"]`)
	if err != nil {
		t.Fatalf("dns aliases failed: %v\n%s", err, h.errOut)
	}
	mu.Lock()
	if most != len(addresses) {
		t.Errorf("%d reverse entries were read at once, want all %d", most, len(addresses))
	}
	for name, n := range reverse {
		if n != 1 {
			t.Errorf("%s was asked %d times, want once", name, n)
		}
	}
	mu.Unlock()
	var got map[string]aliasAnswer
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.out)
	}
	for _, alias := range []string{"submit.hpc.example.org", "login.hpc.example.org"} {
		var hosts []string
		for _, a := range got[alias].Addresses {
			hosts = append(hosts, a.Hosts...)
		}
		if want := "sub0001.hpc.example.org sub0002.hpc.example.org sub0003.hpc.example.org sub0004.hpc.example.org"; strings.Join(hosts, " ") != want {
			t.Errorf("%s leads to %v, want %s", alias, hosts, want)
		}
	}
}

// blackholeDNS takes queries over UDP on the loopback address and answers
// none, as a name server that is down behind a firewall that drops its
// packets. It returns its address and the count of queries it took, and
// calls onQuery, when it is set, as each arrives.
func blackholeDNS(t *testing.T, onQuery func()) (string, *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var asked atomic.Int32
	go func() {
		buf := make([]byte, 512)
		for {
			if _, _, err := conn.ReadFrom(buf); err != nil {
				return
			}
			asked.Add(1)
			if onQuery != nil {
				onQuery()
			}
		}
	}()
	return conn.LocalAddr().String(), &asked
}

// A name server that was down was asked first about every name, twice, and
// each query waited out the timeout before the next server was asked: a
// thousand names took over ten minutes. It is asked last once a query to it
// ran out of time.
func TestDNSAsksAServerThatDidNotAnswerLast(t *testing.T) {
	t.Parallel()
	zone := map[string][]dnsmessage.Resource{}
	var names []string
	for i := range 20 {
		name := fmt.Sprintf("exe%04d.hpc.example.org.", i+1)
		zone[name] = []dnsmessage.Resource{aRR(name, fmt.Sprintf("10.0.0.%d", i+1))}
		names = append(names, name)
	}
	down, asked := blackholeDNS(t, nil)
	c := newDNSServers([]string{down, fakeDNS(t, zone)}, 200*time.Millisecond)
	for i, name := range names {
		_, addresses, err := c.lookupHost(context.Background(), name)
		if want := fmt.Sprintf("10.0.0.%d", i+1); err != nil || !slices.Equal(addresses, []string{want}) {
			t.Fatalf("%s: %v, %v; want %s", name, addresses, err, want)
		}
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("the server that is down was asked %d times, want once", got)
	}
}

// A query the command interrupted says nothing about the server it was
// waiting for, which is still asked first.
func TestDNSAnInterruptedQueryDoesNotDemoteTheServer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	down, _ := blackholeDNS(t, cancel)
	up := fakeDNS(t, map[string][]dnsmessage.Resource{})
	c := newDNSServers([]string{down, up}, time.Minute)
	if _, _, err := c.lookupHost(ctx, "exe0001.hpc.example.org"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the lookup returned %v, want it interrupted", err)
	}
	if got := c.order(); !slices.Equal(got, []int{0, 1}) {
		t.Errorf("the servers are asked in the order %v, want [0 1]", got)
	}
}

// Every UDP exchange read its answer into a buffer of 64 KiB of its own:
// over a gigabyte for a lookup of 10,000 nodes. The buffers are reused.
func TestDNSLookupsReuseTheirReadBuffers(t *testing.T) {
	// The race detector has a pool drop some of what it is given, to
	// find code that counts on getting it back.
	if raceDetector {
		t.Skip("the allocations are checked without the race detector")
	}
	const name = "exe0001.hpc.example.org."
	c := newDNSServers([]string{fakeDNS(t, map[string][]dnsmessage.Resource{
		name: {aRR(name, "10.0.0.1")},
	})}, 5*time.Second)
	lookup := func() {
		if _, _, err := c.lookupHost(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	lookup()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const lookups = 100
	for range lookups {
		lookup()
	}
	runtime.ReadMemStats(&after)
	// A lookup asks for A and AAAA records, two exchanges.
	if per := (after.TotalAlloc - before.TotalAlloc) / lookups; per > 32<<10 {
		t.Errorf("a lookup allocated %d bytes; its read buffers are not reused", per)
	}
}
