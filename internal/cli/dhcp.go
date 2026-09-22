// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/dhcp"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func newDHCPCommand(r *root) *cobra.Command {
	return group("dhcp", "Read what the DHCP server knows about the nodes", `
Read the DHCP server's configuration and log: the addresses, hardware
addresses, client identifiers and boot files it hands to the nodes.

The configuration is parsed rather than grepped, so a declaration whose
options are in an unusual order reports its own values and not a neighbour's.
It is fetched once and reused for a short time, so asking about a hundred
nodes does not fetch it a hundred times.`,
		newDHCPHostsCommand(r),
		newDHCPConfigCommand(r),
		newDHCPLeasesCommand(r),
		newDHCPShellCommand(r),
		newDHCPCaptureCommand(r),
	)
}

// dhcpConfig fetches and parses the server configuration.
func dhcpConfig(a *app.App) (*dhcp.Config, error) {
	spec := a.Spec.Services.DHCP
	if spec.Role == "" {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no host role runs the DHCP server; set services.dhcp.role")
	}
	path := spec.ConfigPath
	if path == "" {
		path = "/etc/dhcp/dhcpd.conf"
	}
	data, err := a.RemoteFile(a.Context(), spec.Role, path, spec.CacheTTL.Or(2*time.Minute))
	if err != nil {
		return nil, err
	}
	cfg, err := dhcp.Parse(data)
	if err != nil {
		return nil, exitcode.Errorf(exitcode.TargetFailed, "parsing %s: %v", path, err)
	}
	return cfg, nil
}

func newDHCPHostsCommand(r *root) *cobra.Command {
	return leaf("hosts [NODESET]", "Show the DHCP entries of a node set", `
Show what the DHCP server hands each node: its address, hardware addresses,
client identifier and boot file. A node with several interfaces has one row
per declaration.

  clusterctl dhcp hosts -n exe[1-4]
  clusterctl dhcp hosts -n exe0001 -o yaml`,
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
			cfg, err := dhcpConfig(a)
			if err != nil {
				return err
			}

			t := output.NewTable(
				output.Column{Name: "NODE"},
				output.Column{Name: "DECLARATION"},
				output.Column{Name: "ADDRESS"},
				output.Column{Name: "MAC"},
				output.Column{Name: "CLIENT ID", Wide: true},
				output.Column{Name: "BOOT FILE"},
			)
			object := map[string][]dhcp.Host{}
			missing := 0
			for _, node := range ns.Expand() {
				hosts := cfg.Lookup(node)
				if len(hosts) == 0 {
					t.Add(node, "not in the DHCP configuration", "", "", "", "")
					missing++
					continue
				}
				object[node] = hosts
				for _, h := range hosts {
					t.Add(node, h.Name, h.Address, strings.Join(h.MACs, ","), h.ClientIdentifier, h.Filename)
				}
			}
			if err := a.Print(output.Result{Table: t, Object: object}); err != nil {
				return err
			}
			if missing > 0 {
				return exitcode.Errorf(exitcode.TargetFailed,
					"%d of %d nodes have no DHCP declaration", missing, ns.Len())
			}
			return nil
		})
}

func newDHCPConfigCommand(r *root) *cobra.Command {
	return leaf("config", "Print the parsed DHCP configuration", `
Print every host declaration the DHCP server carries.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			cfg, err := dhcpConfig(a)
			if err != nil {
				return err
			}
			t := output.NewTable(
				output.Column{Name: "DECLARATION"},
				output.Column{Name: "ADDRESS"},
				output.Column{Name: "MAC"},
				output.Column{Name: "BOOT FILE", Wide: true},
			)
			for _, h := range cfg.Hosts {
				t.Add(h.Name, h.Address, strings.Join(h.MACs, ","), h.Filename)
			}
			t.Caption = fmt.Sprintf("%d host declarations", len(cfg.Hosts))
			return a.Print(output.Result{Table: t, Object: cfg})
		})
}

func newDHCPLeasesCommand(r *root) *cobra.Command {
	var lines int
	cmd := leaf("log", "Show the DHCP responses from the server log", `
Read the DHCP exchanges out of the server's log, which is what to look at when
a node is not coming up.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			spec := a.Spec.Services.DHCP
			if spec.Role == "" {
				return exitcode.Errorf(exitcode.Usage, "no host role runs the DHCP server; set services.dhcp.role")
			}
			path := spec.LogPath
			if path == "" {
				path = "/var/log/syslog"
			}
			result, err := a.RunOnRole(a.Context(), spec.Role, transport.Request{
				Argv:    []string{"sh", "-c", fmt.Sprintf("grep -a dhcpd %s | tail -n %d", path, lines)},
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Output())
			return nil
		})
	cmd.Flags().IntVarP(&lines, "lines", "l", 50, "how many log lines to show")
	return cmd
}

func newDHCPShellCommand(r *root) *cobra.Command {
	return leaf("shell [-- COMMAND...]", "Open a shell on the DHCP server", `
Log in to the DHCP server, or run one command on it.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			return roleShell(r, cmd, args, func(a *app.App) string { return a.Spec.Services.DHCP.Role })
		})
}

func newDHCPCaptureCommand(r *root) *cobra.Command {
	var (
		iface   string
		seconds int
	)
	cmd := leaf("capture", "Capture DHCP traffic on the server", `
Run a bounded packet capture of the DHCP exchange on the server, which is what
to do when a node asks for an address and nothing answers.

The capture stops on its own after the given time, so it cannot be left
running by accident.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			spec := a.Spec.Services.DHCP
			if spec.Role == "" {
				return exitcode.Errorf(exitcode.Usage, "no host role runs the DHCP server; set services.dhcp.role")
			}
			device := iface
			if device == "" {
				device = spec.Interface
			}
			if device == "" {
				device = "any"
			}
			target, err := a.Role(spec.Role)
			if err != nil {
				return err
			}
			req := transport.Request{
				Argv: []string{"tcpdump", "-l", "-n", "-i", device, "-e",
					"port", "67", "or", "port", "68"},
				Timeout: time.Duration(seconds) * time.Second,
				TTY:     transport.TTYNone,
			}
			if a.DryRun() {
				line, err := a.SSH.Args(target, req)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), strings.Join(line, " "))
				return nil
			}
			a.Printf("capturing on %s for %ds\n", device, seconds)
			// The capture is watched live, so it is wired to the terminal
			// rather than collected at the end.
			return a.SSH.Interactive(a.Context(), target, req)
		})
	cmd.Flags().StringVarP(&iface, "interface", "i", "", "interface to capture on (default: from the configuration)")
	cmd.Flags().IntVar(&seconds, "seconds", 60, "how long to capture")
	return cmd
}

// roleShell opens a shell on the host of a configured service role.
func roleShell(r *root, cmd *cobra.Command, args []string, role func(*app.App) string) error {
	a, err := r.App()
	if err != nil {
		return err
	}
	name := role(a)
	if name == "" {
		return exitcode.Errorf(exitcode.Usage, "no host role is configured for this service")
	}
	target, err := a.Role(name)
	if err != nil {
		return err
	}
	var argv []string
	if at := cmd.ArgsLenAtDash(); at >= 0 {
		argv = args[at:]
	}
	req := transport.Request{Argv: argv}
	if a.DryRun() {
		line, err := a.SSH.Args(target, req)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), strings.Join(line, " "))
		return nil
	}
	return a.SSH.Interactive(a.Context(), target, req)
}
