// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"net"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/safety"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func newBootCommand(r *root) *cobra.Command {
	return group("boot", "Configure what the nodes boot over the network", `
The PXE service hands a node a boot configuration when it asks over the
network. These commands read and set which configuration each node gets, and
show the logs of the services involved.

A boot path is configured for one request by default. A persistent path is
what leaves a machine reinstalling every time it reboots, so it has to be
asked for.`,
		newBootStatusCommand(r),
		newBootSetCommand(r),
		newBootUnsetCommand(r),
		newBootListCommand(r),
		newBootSyncCommand(r),
		newBootLogCommand(r),
		newBootShellCommand(r),
		newBootGrubCommand(r),
	)
}

// nodeAddress resolves the address a node boots with: what the inventory
// says, else what DHCP hands it.
func nodeAddress(a *app.App, node string) (string, error) {
	if entry, ok := a.Inventory.Lookup(node); ok && entry.Address != "" {
		return entry.Address, nil
	}
	cfg, err := dhcpConfig(a)
	if err != nil {
		return "", err
	}
	for _, host := range cfg.Lookup(node) {
		if host.Address != "" {
			return host.Address, nil
		}
	}
	return "", exitcode.Errorf(exitcode.Usage,
		"no address is known for %s; set it in the inventory or in DHCP", node)
}

// pxeRole returns the host role the PXE service runs on.
func pxeRole(a *app.App) (string, error) {
	role := a.Spec.Services.PXESrv.Role
	if role == "" {
		return "", exitcode.Errorf(exitcode.Usage,
			"no host role runs the PXE service; set services.pxesrv.role")
	}
	return role, nil
}

func newBootStatusCommand(r *root) *cobra.Command {
	return leaf("status [NODESET]", "Show which boot configuration each node is set to", `
List the boot path configured on the PXE service for each node.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			root := a.Spec.Services.PXESrv.Root
			if root == "" {
				root = "/srv/pxesrv"
			}

			ns, err := a.SelectOptional(strings.Join(args, ","))
			if err != nil {
				return err
			}
			// One listing answers for every node, rather than one connection
			// per node as the shell version did.
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv:    []string{"find", root, "-maxdepth", "1", "-type", "l", "-printf", "%f\t%l\n"},
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			})
			if err != nil {
				return err
			}
			links := map[string]string{}
			for _, line := range result.Lines() {
				name, target, ok := strings.Cut(line, "\t")
				if ok {
					links[name] = target
				}
			}

			t := output.NewTable(output.Cols("NODE", "ADDRESS", "BOOT PATH")...)
			object := map[string]string{}
			if ns == nil {
				for name, target := range links {
					t.Add("", name, target)
					object[name] = target
				}
				t.Caption = fmt.Sprintf("%d boot paths configured on %s", len(links), role)
				return a.Print(output.Result{Table: t, Object: object})
			}
			for _, node := range ns.Expand() {
				address, err := nodeAddress(a, node)
				if err != nil {
					t.Add(node, "unknown", "")
					continue
				}
				target := links[address]
				if target == "" {
					target = "none"
				}
				object[node] = target
				t.Add(node, address, target)
			}
			return a.Print(output.Result{Table: t, Object: object})
		})
}

func newBootSetCommand(r *root) *cobra.Command {
	var persistent bool

	cmd := leaf("set [NODESET] [PATH]", "Set the boot configuration of a node set", `
Point the PXE service at a boot configuration for each node.

Without a path, the boot path rules of the cluster decide, which is what makes
a reinstall a one-liner. A node matched by two rules is an error rather than a
silent first match.

  clusterctl boot set -n exe[1-4]
  clusterctl boot set -n exe0001 /srv/pxesrv/boot/cluster/1.0/exe/ipxe.net2`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			root := a.Spec.Services.PXESrv.Root
			if root == "" {
				root = "/srv/pxesrv"
			}

			var explicit string
			rest := args
			if len(args) > 0 && strings.HasPrefix(args[len(args)-1], "/") {
				explicit = args[len(args)-1]
				rest = args[:len(args)-1]
			}
			ns, err := selection(a, rest)
			if err != nil {
				return err
			}

			// Everything is resolved before anything is written, so a node
			// with no boot path stops the command instead of leaving half
			// the set configured.
			type plan struct{ node, address, path string }
			var plans []plan
			for _, node := range ns.Expand() {
				address, err := nodeAddress(a, node)
				if err != nil {
					return err
				}
				path := explicit
				if path == "" {
					resolved, _, err := inventory.BootPath(a.Inventory, a.Spec.BootPaths, node)
					if err != nil {
						return exitcode.Wrap(exitcode.Usage, err)
					}
					path = resolved
				}
				plans = append(plans, plan{node: node, address: address, path: path})
			}

			mode := "for the next request"
			if persistent {
				mode = "persistently"
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "set the network boot configuration of",
				Targets: ns,
				Detail:  fmt.Sprintf("%s, %s", plans[0].path, mode),
			}); err != nil {
				if safety.IsDryRun(err) {
					return nil
				}
				return err
			}

			var script strings.Builder
			script.WriteString("set -eu\n")
			for _, p := range plans {
				link := root + "/" + p.address
				if persistent {
					link += a.Spec.Services.PXESrv.StaticSuffix
				}
				fmt.Fprintf(&script, "ln -sfn %s %s\n", shellquote.Quote(p.path), shellquote.Quote(link))
			}
			if _, err := a.RunOnRole(a.Context(), role, transport.Request{
				Script:  script.String(),
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			}); err != nil {
				return err
			}

			t := output.NewTable(output.Cols("NODE", "ADDRESS", "BOOT PATH")...)
			for _, p := range plans {
				t.Add(p.node, p.address, p.path)
			}
			return a.Print(output.Result{Table: t})
		})
	cmd.Flags().BoolVar(&persistent, "persistent", false, "keep the boot path after the first request")
	return cmd
}

func newBootUnsetCommand(r *root) *cobra.Command {
	return leaf("unset [NODESET]", "Remove the boot configuration of a node set", `
Remove the boot path of each node, so the PXE service stops offering one.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			root := a.Spec.Services.PXESrv.Root
			if root == "" {
				root = "/srv/pxesrv"
			}
			ns, err := selection(a, args)
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb: "remove the network boot configuration of", Targets: ns,
			}); err != nil {
				if safety.IsDryRun(err) {
					return nil
				}
				return err
			}

			var script strings.Builder
			script.WriteString("set -eu\n")
			for _, node := range ns.Expand() {
				address, err := nodeAddress(a, node)
				if err != nil {
					return err
				}
				fmt.Fprintf(&script, "rm -f %s\n", shellquote.Quote(root+"/"+address))
			}
			if _, err := a.RunOnRole(a.Context(), role, transport.Request{
				Script:  script.String(),
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			}); err != nil {
				return err
			}
			a.Printf("removed the boot configuration of %s\n", ns)
			return nil
		})
}

func newBootListCommand(r *root) *cobra.Command {
	return leaf("list", "List the boot configurations the PXE service offers", `
List the boot configuration files available on the PXE service, which is what
a boot path may point at.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			path := a.Spec.Services.PXESrv.BootPath
			if path == "" {
				path = "/srv/pxesrv/boot"
			}
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv:    []string{"find", path, "-type", "f", "-name", "ipxe.*", "-o", "-type", "f", "-name", "grub.cfg*"},
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			})
			if err != nil {
				return err
			}
			t := output.NewTable(output.Cols("BOOT CONFIGURATION")...)
			for _, line := range result.Lines() {
				t.Add(line)
			}
			t.Caption = fmt.Sprintf("%d configurations under %s", t.Len(), path)
			return a.Print(output.Result{Table: t, Object: result.Lines()})
		})
}

func newBootSyncCommand(r *root) *cobra.Command {
	return leaf("sync", "Update the boot configurations from version control", `
Pull the boot configuration repository on the PXE service, so the
configurations it offers match what is in version control.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			path := a.Spec.Services.PXESrv.RepoPath
			if path == "" {
				return exitcode.Errorf(exitcode.Usage,
					"no boot configuration repository is configured; set services.pxesrv.repoPath")
			}
			if a.DryRun() {
				a.Printf("would run git pull in %s on %s\n", path, role)
				return nil
			}
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv:    []string{"git", "-C", path, "pull", "--ff-only"},
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Output())
			return nil
		})
}

func newBootLogCommand(r *root) *cobra.Command {
	var lines int
	cmd := leaf("log", "Show the PXE service log", `
Read the log of the PXE service, which is what to look at when a node asks
for a boot configuration and does not get the expected one.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role, err := pxeRole(a)
			if err != nil {
				return err
			}
			path := a.Spec.Services.PXESrv.LogPath
			if path == "" {
				path = "/var/log/pxesrv.log"
			}
			result, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv:    []string{"tail", "-n", fmt.Sprint(lines), path},
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

func newBootShellCommand(r *root) *cobra.Command {
	return leaf("shell [-- COMMAND...]", "Open a shell on the PXE service host", `
Log in to the host running the PXE service, or run one command on it.`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			return roleShell(r, cmd, args, func(a *app.App) string { return a.Spec.Services.PXESrv.Role })
		})
}

func newBootGrubCommand(r *root) *cobra.Command {
	set := leaf("set NODE TARGET", "Point a node's GRUB configuration at a target", `
Link the GRUB configuration a node loads over TFTP to an installation target.

GRUB looks for a file named after the node's address in hexadecimal, which is
what this command computes.

  clusterctl boot grub set exe0001 /srv/tftp/grub/1.0/grub.cfg.install-exec`,
		cobra.ExactArgs(2),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			role := a.Spec.Services.TFTP.Role
			if role == "" {
				return exitcode.Errorf(exitcode.Usage, "no host role runs the TFTP service; set services.tftp.role")
			}
			grubPath := a.Spec.Services.TFTP.GrubPath
			if grubPath == "" {
				grubPath = "/srv/tftp/grub"
			}
			address, err := nodeAddress(a, args[0])
			if err != nil {
				return err
			}
			hex, err := addressToHex(address)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			link := grubPath + "/grub.cfg-" + hex

			ns, err := a.Select(args[0])
			if err != nil {
				return err
			}
			if err := a.Gate.Confirm(safety.Action{
				Verb:    "set the GRUB configuration of",
				Targets: ns,
				Detail:  link + " -> " + args[1],
			}); err != nil {
				if safety.IsDryRun(err) {
					return nil
				}
				return err
			}
			if _, err := a.RunOnRole(a.Context(), role, transport.Request{
				Argv:    []string{"ln", "-sfn", args[1], link},
				Timeout: a.Timeout().Get(),
				TTY:     transport.TTYNone,
			}); err != nil {
				return err
			}
			a.Printf("%s now loads %s\n", args[0], args[1])
			return nil
		})

	show := leaf("show NODE", "Show the GRUB file name a node loads", `
Print the address of a node and the GRUB configuration file name it asks for,
which is the address in hexadecimal.`,
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			address, err := nodeAddress(a, args[0])
			if err != nil {
				return err
			}
			hex, err := addressToHex(address)
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			t := output.NewTable(output.Cols("NODE", "ADDRESS", "GRUB FILE")...)
			t.Add(args[0], address, "grub.cfg-"+hex)
			return a.Print(output.Result{Table: t, Object: map[string]string{
				"node": args[0], "address": address, "grubFile": "grub.cfg-" + hex,
			}})
		})

	return group("grub", "Configure what a node loads over TFTP", `
GRUB asks the TFTP service for a configuration named after the node's address
in hexadecimal. These commands compute that name and set the link.`, set, show)
}

// addressToHex renders an IPv4 address the way GRUB asks for it: eight upper
// case hexadecimal digits.
func addressToHex(address string) (string, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return "", fmt.Errorf("%q is not an address", address)
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("%q is not an IPv4 address, which is what GRUB asks for", address)
	}
	return fmt.Sprintf("%02X%02X%02X%02X", v4[0], v4[1], v4[2], v4[3]), nil
}
