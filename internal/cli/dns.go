// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

func newDNSCommand(r *root) *cobra.Command {
	return group("dns", "Resolve the names of the site", `
Resolve host names and the aliases that point at pools of submit nodes.

Lookups ask the name server directly: services.dns.server when it is set,
otherwise the name servers /etc/resolv.conf lists. /etc/hosts and the other
sources of the system resolver are not consulted, because the question is
what DNS says. Every answer is reported, so a name with several addresses,
an address with several names or a chain of aliases is shown as it is rather
than collapsed to the first answer.`,
		newDNSLookupCommand(r),
		newDNSAliasesCommand(r),
	)
}

// resolvConf is where the system name servers are read from.
var resolvConf = "/etc/resolv.conf"

// errNoSuchHost is what a name server says about a name it does not know.
var errNoSuchHost = errors.New("no such host")

// dnsClient asks name servers directly.
//
// The Go resolver answers from /etc/hosts before it asks any server, and it
// reports addresses only, never the aliases it followed to reach them, so
// neither the configured server nor the chain would be what is shown.
type dnsClient struct {
	servers []string
	timeout time.Duration
}

// newDNSClient builds the client from the configured server, or from the
// name servers of the system when none is configured.
func newDNSClient(a *app.App) (*dnsClient, error) {
	timeout := a.Spec.Services.DNS.Timeout.Or(5 * time.Second)
	if server := a.Spec.Services.DNS.Server; server != "" {
		address, err := dnsServerAddress(server)
		if err != nil {
			return nil, exitcode.Wrap(exitcode.Usage, err)
		}
		return &dnsClient{servers: []string{address}, timeout: timeout}, nil
	}
	servers, err := resolvConfServers(resolvConf)
	if err != nil || len(servers) == 0 {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no name server is configured and %s lists none; set services.dns.server", resolvConf)
	}
	return &dnsClient{servers: servers, timeout: timeout}, nil
}

// dnsServerAddress turns a configured server into an address to dial. A
// server without a port gets port 53, and an IPv6 address may be written
// with or without brackets.
func dnsServerAddress(server string) (string, error) {
	if host, port, err := net.SplitHostPort(server); err == nil {
		if host == "" || port == "" {
			return "", fmt.Errorf("services.dns.server %q names no host or no port", server)
		}
		return net.JoinHostPort(host, port), nil
	}
	host := strings.TrimSuffix(strings.TrimPrefix(server, "["), "]")
	if host == "" || strings.ContainsAny(host, "[]/ ") {
		return "", fmt.Errorf("services.dns.server %q is not a host name or address", server)
	}
	return net.JoinHostPort(host, "53"), nil
}

// resolvConfServers reads the nameserver lines of a resolv.conf.
func resolvConfServers(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			out = append(out, net.JoinHostPort(fields[1], "53"))
		}
	}
	return out, scanner.Err()
}

// maxCNAMEs bounds how long a chain of aliases is followed.
const maxCNAMEs = 16

// lookupHost resolves a name to the aliases it passes through and the
// addresses at the end of them.
func (c *dnsClient) lookupHost(ctx context.Context, name string) (chain, addresses []string, err error) {
	missing := 0
	for _, qtype := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		answers, err := c.query(ctx, name, qtype)
		if errors.Is(err, errNoSuchHost) {
			missing++
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		links, end := followCNAMEs(name, answers)
		if len(links) > len(chain) {
			chain = links
		}
		for _, rr := range answers {
			if !sameName(rr.Header.Name.String(), end) {
				continue
			}
			switch body := rr.Body.(type) {
			case *dnsmessage.AResource:
				addresses = append(addresses, net.IP(body.A[:]).String())
			case *dnsmessage.AAAAResource:
				addresses = append(addresses, net.IP(body.AAAA[:]).String())
			}
		}
	}
	if missing == 2 {
		return nil, nil, errNoSuchHost
	}
	if len(addresses) == 0 {
		return chain, nil, errors.New("no address")
	}
	return chain, addresses, nil
}

// lookupAddr resolves an address to every name its reverse entry lists.
func (c *dnsClient) lookupAddr(ctx context.Context, address string) ([]string, error) {
	name, err := reverseName(address)
	if err != nil {
		return nil, err
	}
	answers, err := c.query(ctx, name, dnsmessage.TypePTR)
	if err != nil {
		return nil, err
	}
	// A classless reverse delegation reaches the PTR through an alias.
	_, end := followCNAMEs(name, answers)
	var out []string
	for _, rr := range answers {
		if body, ok := rr.Body.(*dnsmessage.PTRResource); ok && sameName(rr.Header.Name.String(), end) {
			out = append(out, strings.TrimSuffix(body.PTR.String(), "."))
		}
	}
	if len(out) == 0 {
		return nil, errNoSuchHost
	}
	return out, nil
}

// followCNAMEs follows the aliases of name through the answers and returns
// the names it passed through and the name at the end of them.
func followCNAMEs(name string, answers []dnsmessage.Resource) (chain []string, end string) {
	end = absolute(name)
	for range maxCNAMEs {
		next := ""
		for _, rr := range answers {
			if body, ok := rr.Body.(*dnsmessage.CNAMEResource); ok && sameName(rr.Header.Name.String(), end) {
				next = body.CNAME.String()
				break
			}
		}
		if next == "" {
			break
		}
		chain = append(chain, strings.TrimSuffix(next, "."))
		end = next
	}
	return chain, end
}

func absolute(name string) string {
	if strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}

func sameName(a, b string) bool { return strings.EqualFold(absolute(a), absolute(b)) }

// reverseName builds the in-addr.arpa or ip6.arpa name of an address.
func reverseName(address string) (string, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return "", fmt.Errorf("%q is not an address", address)
	}
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa.", v4[3], v4[2], v4[1], v4[0]), nil
	}
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i := len(ip) - 1; i >= 0; i-- {
		b.WriteByte(hex[ip[i]&0xf])
		b.WriteByte('.')
		b.WriteByte(hex[ip[i]>>4])
		b.WriteByte('.')
	}
	b.WriteString("ip6.arpa.")
	return b.String(), nil
}

// query asks the servers in turn and returns the answer section of the first
// one that answers.
func (c *dnsClient) query(ctx context.Context, name string, qtype dnsmessage.Type) ([]dnsmessage.Resource, error) {
	qname, err := dnsmessage.NewName(absolute(name))
	if err != nil {
		return nil, fmt.Errorf("%q is not a DNS name: %w", name, err)
	}
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	question := dnsmessage.Question{Name: qname, Type: qtype, Class: dnsmessage.ClassINET}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{question},
	}
	packed, err := msg.Pack()
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, server := range c.servers {
		resp, err := c.exchange(ctx, "udp", server, packed, msg.ID, question)
		if err == nil && resp.Truncated {
			resp, err = c.exchange(ctx, "tcp", server, packed, msg.ID, question)
		}
		if err != nil {
			lastErr = err
			continue
		}
		switch resp.RCode {
		case dnsmessage.RCodeSuccess:
			return resp.Answers, nil
		case dnsmessage.RCodeNameError:
			return nil, errNoSuchHost
		default:
			// A server that fails or refuses says nothing about the name;
			// the next one may know.
			lastErr = fmt.Errorf("%s answered %s", server, strings.TrimPrefix(resp.RCode.String(), "RCode"))
		}
	}
	return nil, lastErr
}

// exchange sends one query to one server and waits for the answer to it.
// It is reported as a call, which says what was asked of which server; an
// answer that the name does not exist is an answer, and ends it ok.
func (c *dnsClient) exchange(ctx context.Context, network, server string, packed []byte, id uint16,
	question dnsmessage.Question) (resp *dnsmessage.Message, err error) {
	asked := strings.TrimPrefix(question.Type.String(), "Type") + " " + strings.TrimSuffix(question.Name.String(), ".")
	if network == "tcp" {
		asked += " over tcp"
	}
	ctx, call := progress.Start(ctx, progress.KindCall, "dns", progress.Host(server),
		progress.Timeout(c.timeout), progress.Message(asked))
	defer func() { call.End(err) }()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// A cancelled command closes the connection, which ends a blocked read.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if network == "tcp" {
		framed := make([]byte, 2+len(packed))
		binary.BigEndian.PutUint16(framed, uint16(len(packed)))
		copy(framed[2:], packed)
		if _, err := conn.Write(framed); err != nil {
			return nil, err
		}
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return nil, err
		}
		buf := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return nil, err
		}
		return checkAnswer(buf, id, question)
	}

	if _, err := conn.Write(packed); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			// The connection's deadline is the context's, and either may
			// be seen first.
			if cause := ctx.Err(); cause != nil || errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, &noAnswer{server: server, timeout: c.timeout, err: cmp.Or(cause, context.DeadlineExceeded)}
			}
			return nil, err
		}
		// A datagram that is not the answer to this question is ignored,
		// as the system resolver does.
		if resp, err := checkAnswer(buf[:n], id, question); err == nil {
			return resp, nil
		}
	}
}

// noAnswer is a server that did not answer before the query's time was up,
// or before the command was interrupted. It unwraps to the context's error,
// which tells the two apart, so that a display reads the first as a timeout
// and the second as canceled.
type noAnswer struct {
	server  string
	timeout time.Duration
	err     error
}

func (e *noAnswer) Error() string {
	if errors.Is(e.err, context.Canceled) {
		return fmt.Sprintf("the query to %s was interrupted before it was answered", e.server)
	}
	return fmt.Sprintf("%s did not answer within %s", e.server, e.timeout)
}

func (e *noAnswer) Unwrap() error { return e.err }

// checkAnswer parses a response and makes sure it answers the question.
func checkAnswer(data []byte, id uint16, question dnsmessage.Question) (*dnsmessage.Message, error) {
	var resp dnsmessage.Message
	if err := resp.Unpack(data); err != nil {
		return nil, err
	}
	if !resp.Response || resp.ID != id || len(resp.Questions) != 1 ||
		!sameName(resp.Questions[0].Name.String(), question.Name.String()) ||
		resp.Questions[0].Type != question.Type {
		return nil, errors.New("the answer is not for this question")
	}
	return &resp, nil
}

// dnsAnswer is what dns lookup reports for one host name.
type dnsAnswer struct {
	CNAMEs    []string `json:"cnames,omitempty" yaml:"cnames,omitempty"`
	Addresses []string `json:"addresses" yaml:"addresses"`
	// Literal says the host is an address, which was not looked up.
	Literal bool `json:"literal,omitempty" yaml:"literal,omitempty"`
}

func newDNSLookupCommand(r *root) *cobra.Command {
	var bmc bool

	cmd := leaf("lookup [NODESET]", "Resolve the host names of a node set", `
Resolve each node's host name and print every address that comes back, and
every alias passed through on the way.

With --bmc, each node's service processor is resolved instead: the
bmcAddress the inventory records for it, else the name the naming rules
give it, which is the host the bmc commands reach. A bmcAddress that is an
address is shown as it is and not looked up.

The names are resolved services.dns.maxConcurrent at a time, 16 by
default, or fewer when --fanout is lower, and listed in the order of the
nodes whatever order the answers came in.

  clusterctl dns lookup -n exe[1-4]
  clusterctl dns lookup -n exe[1-4] --bmc`,
		cobra.ArbitraryArgs,
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			res, err := newDNSClient(a)
			if err != nil {
				return err
			}

			// Every node's host is worked out before any name is looked
			// up, so that a node the rules cannot name stops the command
			// before a server is asked anything.
			nodes := ns.Expand()
			hosts := make([]string, len(nodes))
			var lookups []int
			for i, node := range nodes {
				if bmc {
					hosts[i], err = a.BMCHost(node)
				} else {
					hosts[i], err = a.Namer.FQDN(node)
				}
				if err != nil {
					return exitcode.Wrap(exitcode.Usage, err)
				}
				// A bmcAddress may be an address rather than a name. It is
				// what the bmc commands reach, so it is shown as it is; no
				// server is asked, and the note says so, so that it is not
				// taken for an answer.
				if net.ParseIP(hosts[i]) != nil {
					a.Printf("%s: %s is an address, not a name; it was not looked up\n", node, hosts[i])
					continue
				}
				lookups = append(lookups, i)
			}

			ctx := a.Context()
			outcomes := fanout.Map(ctx, lookups, fanout.Options[int]{
				Step:  "resolve the names",
				Limit: a.Bound(a.Spec.Services.DNS.MaxConcurrent),
				Describe: func(i int) (node, host, role string) {
					return nodes[i], hosts[i], ""
				},
				PanicLog: a.Diag,
			}, func(ctx context.Context, i int) (dnsAnswer, error) {
				chain, addresses, err := res.lookupHost(ctx, hosts[i])
				return dnsAnswer{CNAMEs: chain, Addresses: addresses}, err
			})
			if err := ctx.Err(); err != nil {
				return err
			}

			// The answers are listed in the order of the nodes.
			answers := make([]*fanout.Outcome[dnsAnswer], len(nodes))
			for k, i := range lookups {
				answers[i] = &outcomes[k]
			}
			t := output.NewTable(output.Cols("NODE", "HOST", "CNAME", "ADDRESSES")...)
			object := map[string]dnsAnswer{}
			failed := 0
			for i, node := range nodes {
				host, answer := hosts[i], answers[i]
				switch {
				case answer == nil:
					object[host] = dnsAnswer{Addresses: []string{host}, Literal: true}
					t.Add(node, host, "", host)
				case answer.Err != nil:
					t.Add(node, host, strings.Join(answer.Value.CNAMEs, " -> "), "no answer: "+answer.Err.Error())
					failed++
				default:
					object[host] = answer.Value
					t.Add(node, host, strings.Join(answer.Value.CNAMEs, " -> "), strings.Join(answer.Value.Addresses, ", "))
				}
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d names did not resolve", failed, ns.Len())
			}
			return nil
		}))
	cmd.Flags().BoolVarP(&bmc, "bmc", "b", false, "resolve the service processors instead")
	return cmd
}

// aliasAnswer is what dns aliases reports for one alias.
type aliasAnswer struct {
	CNAMEs    []string       `json:"cnames,omitempty" yaml:"cnames,omitempty"`
	Addresses []aliasAddress `json:"addresses" yaml:"addresses"`
}

// aliasAddress is one address behind an alias and every name it has.
type aliasAddress struct {
	Address string   `json:"address" yaml:"address"`
	Hosts   []string `json:"hosts" yaml:"hosts"`
}

func newDNSAliasesCommand(r *root) *cobra.Command {
	return leaf("aliases", "Resolve the configured aliases to the hosts behind them", `
Resolve the site's aliases, such as the name users log in to, and report every
host behind each of them: the chain of names the alias passes through, each
address at its end, and every name the reverse entry of that address lists.

An alias that resolves to several machines is what a login pool looks like;
this is how to see which machines are currently in it.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			aliases := a.Spec.Services.DNS.Aliases
			if len(aliases) == 0 {
				return exitcode.Errorf(exitcode.Usage,
					"no aliases are configured; add services.dns.aliases to the site document")
			}
			res, err := newDNSClient(a)
			if err != nil {
				return err
			}
			domain := a.Namer.Domain("hpc")

			t := output.NewTable(output.Cols("ALIAS", "CNAME", "ADDRESS", "HOSTS")...)
			object := map[string]aliasAnswer{}
			failed := 0
			for _, alias := range aliases {
				name := alias
				if !strings.Contains(name, ".") && domain != "" {
					name += "." + domain
				}
				chain, addresses, err := res.lookupHost(a.Context(), name)
				if err != nil {
					if ctxErr := a.Context().Err(); ctxErr != nil {
						return ctxErr
					}
					t.Add(name, strings.Join(chain, " -> "), "no answer", err.Error())
					failed++
					continue
				}
				answer := aliasAnswer{CNAMEs: chain}
				for _, address := range addresses {
					// The reverse lookup turns an address back into the
					// machines behind the alias, which is the answer that
					// matters here.
					hosts, err := res.lookupAddr(a.Context(), address)
					shown := strings.Join(hosts, ", ")
					if err != nil && !errors.Is(err, errNoSuchHost) {
						shown = "no answer: " + err.Error()
					}
					if hosts == nil {
						hosts = []string{}
					}
					t.Add(name, strings.Join(chain, " -> "), address, shown)
					answer.Addresses = append(answer.Addresses, aliasAddress{Address: address, Hosts: hosts})
				}
				object[name] = answer
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d aliases did not resolve", failed)
			}
			return nil
		}))
}
