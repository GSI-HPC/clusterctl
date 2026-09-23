// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
)

func newDNSCommand(r *root) *cobra.Command {
	return group("dns", "Resolve the names of the site", `
Resolve host names and the aliases that point at pools of submit nodes.

Lookups go through a resolver that reports every answer, so a name with
several addresses or a chain of aliases is shown as it is rather than
collapsed to the first answer.`,
		newDNSLookupCommand(r),
		newDNSAliasesCommand(r),
	)
}

// resolver builds the resolver, honouring a configured server.
func resolver(a *app.App) *net.Resolver {
	server := a.Spec.Services.DNS.Server
	timeout := a.Spec.Services.DNS.Timeout.Or(5 * time.Second)
	if server == "" {
		return net.DefaultResolver
	}
	if !strings.Contains(server, ":") {
		server = net.JoinHostPort(server, "53")
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, server)
		},
	}
}

func newDNSLookupCommand(r *root) *cobra.Command {
	var bmc bool

	cmd := leaf("lookup [NODESET]", "Resolve the host names of a node set", `
Resolve each node's host name and print every address that comes back.

  clusterctl dns lookup -n exe[1-4]
  clusterctl dns lookup -n exe[1-4] --bmc`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			res := resolver(a)

			t := output.NewTable(output.Cols("NODE", "HOST", "ADDRESSES")...)
			object := map[string][]string{}
			failed := 0
			for _, node := range ns.Expand() {
				var host string
				if bmc {
					host, err = a.Namer.BMC(node)
				} else {
					host, err = a.Namer.FQDN(node)
				}
				if err != nil {
					return exitcode.Wrap(exitcode.Usage, err)
				}
				addresses, err := res.LookupHost(a.Context(), host)
				if err != nil {
					t.Add(node, host, "no answer: "+cleanDNSError(err))
					failed++
					continue
				}
				object[host] = addresses
				t.Add(node, host, strings.Join(addresses, ", "))
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d of %d names did not resolve", failed, ns.Len())
			}
			return nil
		})
	cmd.Flags().BoolVarP(&bmc, "bmc", "b", false, "resolve the service processor names instead")
	return cmd
}

func newDNSAliasesCommand(r *root) *cobra.Command {
	return leaf("aliases", "Resolve the configured aliases to the hosts behind them", `
Resolve the site's aliases, such as the name users log in to, and report every
host behind each of them.

An alias that resolves to several machines is what a login pool looks like;
this is how to see which machines are currently in it.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			aliases := a.Spec.Services.DNS.Aliases
			if len(aliases) == 0 {
				return exitcode.Errorf(exitcode.Usage,
					"no aliases are configured; add services.dns.aliases to the site document")
			}
			res := resolver(a)
			domain := a.Namer.Domain("hpc")

			t := output.NewTable(output.Cols("ALIAS", "ADDRESS", "HOST")...)
			object := map[string][]map[string]string{}
			failed := 0
			for _, alias := range aliases {
				name := alias
				if !strings.Contains(name, ".") && domain != "" {
					name += "." + domain
				}
				addresses, err := res.LookupHost(a.Context(), name)
				if err != nil {
					t.Add(name, "no answer", cleanDNSError(err))
					failed++
					continue
				}
				for _, address := range addresses {
					// The reverse lookup turns an address back into the
					// machine behind the alias, which is the answer that
					// matters here.
					hosts, err := res.LookupAddr(a.Context(), address)
					host := ""
					if err == nil && len(hosts) > 0 {
						host = strings.TrimSuffix(hosts[0], ".")
					}
					t.Add(name, address, host)
					object[name] = append(object[name], map[string]string{"address": address, "host": host})
				}
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.Errorf(exitcode.TargetFailed, "%d aliases did not resolve", failed)
			}
			return nil
		})
}

// cleanDNSError strips the repeated wrapper the resolver adds, which
// otherwise buries the cause under the whole query it was answering.
func cleanDNSError(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.Err
	}
	return err.Error()
}
