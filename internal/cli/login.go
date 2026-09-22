// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// loginFlags are the connection options shared by login and the commands
// that open a shell on a service host.
type loginFlags struct {
	user      string
	root      bool
	agent     bool
	x11       bool
	tty       bool
	noTTY     bool
	jump      string
	sshDebug  bool
	sshOption []string
}

func (f *loginFlags) register(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVarP(&f.user, "user", "u", "", "remote account to log in as")
	flags.BoolVarP(&f.root, "root", "r", false, "log in as root")
	flags.BoolVarP(&f.agent, "forward-agent", "A", false, "forward the ssh agent")
	flags.BoolVarP(&f.x11, "x11", "X", false, "forward X11")
	flags.BoolVarP(&f.tty, "tty", "t", false, "force a terminal even for a command")
	flags.BoolVarP(&f.noTTY, "no-tty", "T", false, "never allocate a terminal")
	flags.StringVarP(&f.jump, "jump", "J", "", "role or host to jump through")
}

// apply folds the flags into a target.
func (f *loginFlags) apply(t transport.Target) transport.Target {
	if f.user != "" {
		t.User = f.user
	}
	if f.root {
		t.User = "root"
	}
	if f.agent {
		t.ForwardAgent = true
	}
	if f.x11 {
		t.ForwardX11 = true
	}
	return t
}

// ttyMode resolves the terminal flags. Forcing and forbidding at once is a
// mistake worth reporting rather than resolving silently.
func (f *loginFlags) ttyMode() (transport.TTY, error) {
	switch {
	case f.tty && f.noTTY:
		return transport.TTYAuto, exitcode.Errorf(exitcode.Usage, "-t and -T contradict each other")
	case f.tty:
		return transport.TTYForce, nil
	case f.noTTY:
		return transport.TTYNone, nil
	default:
		return transport.TTYAuto, nil
	}
}

// resolveTarget turns a positional argument into a target: a configured role
// name when it matches one, and a host name otherwise.
func resolveTarget(a *app.App, name string) (transport.Target, error) {
	if name == "" {
		return a.Role(defaultRole(a))
	}
	if _, ok := a.Spec.Hosts[name]; ok {
		return a.Role(name)
	}
	if strings.Contains(name, ".") {
		return transport.Target{Name: name, Host: name}, nil
	}
	// A bare name that is neither a role nor qualified is treated as a node,
	// so that "login exe0001" reaches a compute node.
	return a.Node(name)
}

// defaultRole is the role login connects to when none is named.
func defaultRole(a *app.App) string {
	for _, candidate := range []string{"login", "mgmt"} {
		if _, ok := a.Spec.Hosts[candidate]; ok {
			return candidate
		}
	}
	names := a.RoleNames()
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

func newLoginCommand(r *root) *cobra.Command {
	f := &loginFlags{}

	cmd := leaf("login [ROLE|HOST|NODE] [-- COMMAND...]", "Open a shell on a host, or run one command there", `
Log in to an infrastructure role, a host or a node.

With no argument the login role is used. A name that matches a configured
role connects to that role with its own account and options; any other name
is taken as a host or a node and resolved through the naming rules.

Everything after -- is run on the remote host instead of opening a shell. The
argument vector is quoted once and reassembled by the remote shell exactly as
it was given, so globs, quotes and whitespace survive:

  clusterctl login install -- ls '/srv/pxesrv/boot/*'
  clusterctl login -r mgmt
  clusterctl login exe0001`,
		cobra.ArbitraryArgs,
		func(cmd *cobra.Command, args []string) error {
			a, err := r.App()
			if err != nil {
				return err
			}
			name := ""
			// Everything after -- is the remote command; cobra hands both
			// halves over, so the split is found here.
			argv := args
			if at := cmd.ArgsLenAtDash(); at >= 0 {
				if at > 0 {
					name = args[0]
				}
				argv = args[at:]
			} else {
				if len(args) > 0 {
					name = args[0]
				}
				argv = nil
			}

			target, err := resolveTarget(a, name)
			if err != nil {
				return err
			}
			target = f.apply(target)
			if f.jump != "" {
				return exitcode.Errorf(exitcode.Usage,
					"-J is configuration, not a flag: give the role a proxyJump in the site document")
			}

			tty, err := f.ttyMode()
			if err != nil {
				return err
			}
			req := transport.Request{Argv: argv, TTY: tty}

			if a.DryRun() {
				line, err := a.SSH.Args(target, req)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", strings.Join(line, " "))
				return nil
			}
			return a.SSH.Interactive(a.Context(), target, req)
		})

	f.register(cmd)
	cmd.ValidArgsFunction = completeRoles(r)
	return cmd
}
