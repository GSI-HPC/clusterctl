// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/internal/tunnel"
)

func newTunnelCommand(r *root) *cobra.Command {
	return group("tunnel", "Manage the sshuttle tunnels into the site networks", `
A tunnel routes one or more site networks through an infrastructure host, so
that addresses behind it can be reached directly. The profiles come from the
site configuration, which resolves network names and this workstation's own
address rather than repeating them in a script.`,
		newTunnelListCommand(r),
		newTunnelStartCommand(r),
		newTunnelStopCommand(r),
		newTunnelStatusCommand(r),
	)
}

// manager builds the tunnel manager from the resolved configuration.
func manager(a *app.App) *tunnel.Manager {
	vars := map[string]string{"workstation.host": a.Spec.Workstation.Host}
	for name, address := range a.Spec.Workstation.Addresses {
		vars["workstation.addresses."+name] = address
	}
	return &tunnel.Manager{
		Profiles: a.Spec.Tunnels,
		Networks: a.Spec.Networks,
		StateDir: a.StateDir,
		Binary:   a.Spec.Workstation.SshuttleBinary,
		Vars:     vars,
		SSH:      a.SSH.Command,
		Destination: func(remote, user string) (string, error) {
			// A profile may name a host directly rather than a role, so a
			// name that is not a role is used as it is.
			target := transport.Target{Name: remote, Host: remote, User: user}
			if _, ok := a.Spec.Hosts[remote]; ok {
				role, err := a.Role(remote)
				if err != nil {
					return "", err
				}
				target.Host, target.Role = role.Host, role.Name
			}
			return a.SSH.Destination(target)
		},
	}
}

func newTunnelListCommand(r *root) *cobra.Command {
	return leaf("list", "List the configured tunnels", `
List the tunnel profiles the site offers and what each one routes.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			m := manager(a)
			t := output.NewTable(
				output.Column{Name: "NAME"},
				output.Column{Name: "REMOTE"},
				output.Column{Name: "SUBNETS"},
				output.Column{Name: "DESCRIPTION", Wide: true},
			)
			for _, s := range m.Status() {
				t.Add(s.Name, s.Remote, s.Subnets, s.Description)
			}
			return a.Print(output.Result{Table: t, Object: m.Status()})
		}))
}

func newTunnelStatusCommand(r *root) *cobra.Command {
	return leaf("status", "Show which tunnels are up", `
Report every profile and whether it is running.

A process id file left behind by a crash is not reported as a running tunnel:
the process it names has to be alive and running with that file, as sshuttle
started by tunnel start does.`,
		cobra.NoArgs,
		r.run(func(a *app.App, cmd *cobra.Command, _ []string) error {
			status := manager(a).Status()
			t := output.NewTable(
				output.Column{Name: "NAME"},
				output.Column{Name: "STATE"},
				output.Column{Name: "PID", Right: true},
				output.Column{Name: "REMOTE"},
				output.Column{Name: "SUBNETS", Wide: true},
			)
			for _, s := range status {
				state, pid := "down", ""
				if s.Running {
					state, pid = "up", fmt.Sprint(s.PID)
				}
				t.Add(s.Name, state, pid, s.Remote, s.Subnets)
			}
			return a.Print(output.Result{Table: t, Object: status})
		}))
}

func newTunnelStartCommand(r *root) *cobra.Command {
	cmd := leaf("start NAME", "Bring a tunnel up", `
Start a tunnel profile. sshuttle changes the local firewall, so it may ask
for the local password.

sshuttle connects with the generated ssh configuration, like every other
connection: the host key is checked against the site's file, and the role's
jump hosts and account apply. An exclude that expands to nothing is refused.`,
		cobra.ExactArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			m := manager(a)
			argv, err := m.Args(args[0])
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			if a.DryRun() {
				// The ssh command is one argument with spaces in it, so
				// the preview is quoted to be pasted into a shell.
				return say(cmd, "%s\n", shellquote.Join(argv))
			}
			if err := m.Start(a.Context(), args[0]); err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			a.Printf("tunnel %s is up\n", args[0])
			return nil
		}))
	cmd.ValidArgsFunction = completeTunnels(r)
	return cmd
}

func newTunnelStopCommand(r *root) *cobra.Command {
	cmd := leaf("stop NAME", "Bring a tunnel down", `
Stop a running tunnel profile. Only the process running with the profile's
process id file is signalled; a stale file is removed.`,
		cobra.ExactArgs(1),
		r.run(func(a *app.App, cmd *cobra.Command, args []string) error {
			m := manager(a)
			if _, err := m.Profile(args[0]); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			if a.DryRun() {
				a.Printf("would stop tunnel %s\n", args[0])
				return nil
			}
			if err := m.Stop(a.Context(), args[0]); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			a.Printf("tunnel %s is down\n", args[0])
			return nil
		}))
	cmd.ValidArgsFunction = completeTunnels(r)
	return cmd
}

func completeTunnels(r *root) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return complete(r, func(a *app.App) []string { return manager(a).Names() })
}
