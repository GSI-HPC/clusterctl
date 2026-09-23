// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/output"
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
		Host: func(role string) (string, string, error) {
			// A profile may name a host directly rather than a role, so a
			// name that is not a role is used as it is.
			if _, ok := a.Spec.Hosts[role]; !ok {
				return role, "", nil
			}
			target, err := a.Role(role)
			if err != nil {
				return "", "", err
			}
			return target.Host, target.User, nil
		},
	}
}

func newTunnelListCommand(r *root) *cobra.Command {
	return leaf("list", "List the configured tunnels", `
List the tunnel profiles the site offers and what each one routes.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
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
		})
}

func newTunnelStatusCommand(r *root) *cobra.Command {
	return leaf("status", "Show which tunnels are up", `
Report every profile and whether it is running.

A process id file left behind by a crash is not reported as a running tunnel:
the process is checked as well.`,
		cobra.NoArgs,
		func(cmd *cobra.Command, _ []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
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
		})
}

func newTunnelStartCommand(r *root) *cobra.Command {
	cmd := leaf("start NAME", "Bring a tunnel up", `
Start a tunnel profile. sshuttle changes the local firewall, so it may ask
for the local password.`,
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			m := manager(a)
			argv, err := m.Args(args[0])
			if err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			if a.DryRun() {
				return say(cmd, "%s\n", strings.Join(argv, " "))
			}
			if err := m.Start(a.Context(), args[0]); err != nil {
				return exitcode.Wrap(exitcode.Transport, err)
			}
			a.Printf("tunnel %s is up\n", args[0])
			return nil
		})
	cmd.ValidArgsFunction = completeTunnels(r)
	return cmd
}

func newTunnelStopCommand(r *root) *cobra.Command {
	cmd := leaf("stop NAME", "Bring a tunnel down", `
Stop a running tunnel profile.`,
		cobra.ExactArgs(1),
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			if a.DryRun() {
				a.Printf("would stop tunnel %s\n", args[0])
				return nil
			}
			if err := manager(a).Stop(args[0]); err != nil {
				return exitcode.Wrap(exitcode.Usage, err)
			}
			a.Printf("tunnel %s is down\n", args[0])
			return nil
		})
	cmd.ValidArgsFunction = completeTunnels(r)
	return cmd
}

func completeTunnels(r *root) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		a, err := r.App()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return manager(a).Names(), cobra.ShellCompDirectiveNoFileComp
	}
}
