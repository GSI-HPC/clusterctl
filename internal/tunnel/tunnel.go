// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package tunnel manages the sshuttle profiles a site offers.
//
// A profile says which networks to route, through which host, and what to
// keep off the tunnel. It replaces the per site shell scripts that carried
// the same arguments, and it resolves network names and the workstation's own
// address from the configuration instead of hard coding them.
package tunnel

import (
	"context"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/tmpl"
)

// Manager starts, stops and reports on the tunnels of a site.
type Manager struct {
	// Profiles are the configured tunnels.
	Profiles map[string]v1alpha1.TunnelSpec
	// Networks maps a network name to a CIDR.
	Networks map[string]string
	// Destination resolves a profile's remote, a role name or a host, and
	// the account it names, if any, to the [user@]host sshuttle connects
	// to. The account is resolved the way every other connection resolves
	// it, so that the context's account applies where nothing names one.
	Destination func(remote, user string) (string, error)
	// SSH returns the ssh command sshuttle runs, with the generated
	// configuration: the site's host key file, strict checking and the
	// role's jump hosts reach the tunnel only through it.
	SSH func() ([]string, error)
	// StateDir holds the process id files. It is an absolute path: sshuttle
	// changes to / once it runs in the background.
	StateDir string
	// Binary is the sshuttle to run.
	Binary string
	// Vars are the template values a profile may refer to, such as
	// {workstation.host}.
	Vars map[string]string
}

// Names lists the configured profiles in sorted order.
func (m *Manager) Names() []string {
	return slices.Sorted(maps.Keys(m.Profiles))
}

// Args builds the sshuttle argument vector for a profile.
//
// The options clusterctl decides come after the profile's own, because
// sshuttle keeps the last value it is given for an option: an option of the
// profile can neither replace the ssh command, and with it the site's host
// key checking, nor the process id file the tunnel is found by.
func (m *Manager) Args(name string) ([]string, error) {
	profile, err := m.Profile(name)
	if err != nil {
		return nil, err
	}
	if len(profile.Subnets) == 0 {
		return nil, fmt.Errorf("tunnel %q routes no subnet", name)
	}
	for _, option := range profile.Options {
		if fixed, ok := fixedOption(option); ok {
			return nil, fmt.Errorf("tunnel %q: the option %q is refused: clusterctl sets %s itself", name, option, fixed)
		}
	}

	remote := profile.Remote
	if profile.User != "" {
		remote = profile.User + "@" + remote
	}
	if m.Destination != nil {
		remote, err = m.Destination(profile.Remote, profile.User)
		if err != nil {
			return nil, fmt.Errorf("tunnel %q: %w", name, err)
		}
	}
	if m.SSH == nil {
		// Plain ssh would read the administrator's own known_hosts rather
		// than the site's file.
		return nil, fmt.Errorf("tunnel %q: no ssh command with the site's configuration was given", name)
	}
	ssh, err := m.SSH()
	if err != nil {
		return nil, fmt.Errorf("tunnel %q: %w", name, err)
	}

	binary := m.Binary
	if binary == "" {
		binary = "sshuttle"
	}
	args := []string{binary}
	if profile.DNS {
		args = append(args, "--dns")
	}
	if profile.Method != "" {
		args = append(args, "--method", profile.Method)
	}
	for _, exclude := range profile.Excludes {
		value, err := m.resolve(exclude)
		if err != nil {
			return nil, fmt.Errorf("tunnel %q: exclude %q: %w", name, exclude, err)
		}
		if value == "" {
			// Dropping it would route what it was meant to keep off the
			// tunnel, such as this machine's own address.
			return nil, fmt.Errorf("tunnel %q: exclude %q expands to nothing; define the value it refers to, such as the host of a Workstation document for this machine, or remove the exclude", name, exclude)
		}
		args = append(args, "--exclude", value)
	}
	args = append(args, profile.Options...)
	// sshuttle splits the ssh command the way a POSIX shell does, so a
	// space in the path of the generated file stays inside one word.
	args = append(args,
		"--daemon", "--pidfile", m.PIDFile(name),
		"--ssh-cmd", shellquote.Join(ssh),
		"--remote", remote)
	for _, subnet := range profile.Subnets {
		value, err := m.resolve(subnet)
		if err != nil {
			return nil, fmt.Errorf("tunnel %q: subnet %q: %w", name, subnet, err)
		}
		if err := validateSubnet(value); err != nil {
			return nil, fmt.Errorf("tunnel %q: subnet %q: %w", name, subnet, err)
		}
		args = append(args, value)
	}
	return args, nil
}

// Profile returns a configured profile. Every operation goes through here,
// so that a name, which also names the process id file, is never a path.
func (m *Manager) Profile(name string) (v1alpha1.TunnelSpec, error) {
	profile, ok := m.Profiles[name]
	if !ok {
		return profile, fmt.Errorf("unknown tunnel %q; the site defines %s", name, strings.Join(m.Names(), ", "))
	}
	return profile, nil
}

// fixedOptions are the sshuttle options clusterctl sets for every tunnel,
// with their short forms.
var fixedOptions = []struct{ long, short string }{
	{"--daemon", "-D"},
	{"--pidfile", ""},
	{"--ssh-cmd", "-e"},
	{"--remote", "-r"},
}

// fixedOption reports whether a profile option sets one of the fixed
// options, in any of the spellings sshuttle's parser accepts: the short
// form, with its value attached or not, and any abbreviation of the long
// one.
func fixedOption(option string) (string, bool) {
	name, _, _ := strings.Cut(option, "=")
	for _, fixed := range fixedOptions {
		switch {
		case strings.HasPrefix(name, "--") && len(name) > 2 && strings.HasPrefix(fixed.long, name):
			return fixed.long, true
		case fixed.short != "" && !strings.HasPrefix(option, "--") && strings.HasPrefix(option, fixed.short):
			return fixed.long, true
		}
	}
	return "", false
}

// resolve turns a network name or a template into a CIDR or a host.
func (m *Manager) resolve(value string) (string, error) {
	if strings.ContainsAny(value, "{") {
		expanded, err := tmpl.Expand(value, m.Vars)
		if err != nil {
			return "", err
		}
		value = expanded
	}
	if cidr, ok := m.Networks[value]; ok {
		return cidr, nil
	}
	return value, nil
}

// validateSubnet checks that a routed subnet is an address or a network.
//
// A name that is neither a configured network nor an address is almost always
// a typo, and sshuttle would either route nothing or, worse, route something
// else, so it is reported here.
func validateSubnet(value string) error {
	// sshuttle allows a port or a port range after the network.
	network := value
	if i := strings.LastIndexByte(network, ':'); i >= 0 && strings.Count(network, ":") == 1 {
		network = network[:i]
	}
	if _, _, err := net.ParseCIDR(network); err == nil {
		return nil
	}
	if net.ParseIP(network) != nil {
		return nil
	}
	return fmt.Errorf("it is neither a configured network name nor an address or a network in CIDR form")
}

// PIDFile is where a running profile records its process id.
func (m *Manager) PIDFile(name string) string {
	return filepath.Join(m.StateDir, "tunnels", name+".pid")
}

// Status describes one profile.
type Status struct {
	Name        string `json:"name" yaml:"name"`
	Running     bool   `json:"running" yaml:"running"`
	PID         int    `json:"pid,omitempty" yaml:"pid,omitempty"`
	Remote      string `json:"remote" yaml:"remote"`
	Subnets     string `json:"subnets" yaml:"subnets"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Status reports every profile and whether it is running.
func (m *Manager) Status() []Status {
	out := make([]Status, 0, len(m.Profiles))
	for _, name := range m.Names() {
		profile := m.Profiles[name]
		pid, running := m.running(name)
		out = append(out, Status{
			Name:        name,
			Running:     running,
			PID:         pid,
			Remote:      profile.Remote,
			Subnets:     strings.Join(profile.Subnets, ","),
			Description: profile.Description,
		})
	}
	return out
}

// running reads the process id file and checks that the process is this
// profile's tunnel. The file outlives a crash, and the number in it can by
// now belong to any other process of this user, so being alive is not
// enough: the process has to be running with this very process id file.
//
// The process id is returned even when it is not the tunnel's, so that the
// stale file can be removed.
func (m *Manager) running(name string) (int, bool) {
	path := m.PIDFile(name)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return pid, false
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return pid, false
	}
	return pid, runsWith(pid, path)
}

// runsWith reports whether a process was started with --pidfile path, the
// way Args starts every tunnel. sshuttle may be a wrapper the workstation
// names, so its name is not checked; the process id file lives in this
// user's private state directory, and nothing but the tunnel is started
// with it.
func runsWith(pid int, path string) bool {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if err != nil {
			return false
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		for i, arg := range args {
			if arg == "--pidfile="+path || arg == "--pidfile" && i+1 < len(args) && args[i+1] == path {
				return true
			}
		}
		return false
	}
	// Elsewhere ps is what tells the command line, with the arguments
	// joined by spaces.
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	line := " " + strings.TrimSpace(string(out)) + " "
	return strings.Contains(line, " --pidfile "+path+" ") || strings.Contains(line, " --pidfile="+path+" ")
}

// lock takes the lock on a profile's process id file, so that two runs
// cannot start the same tunnel, or stop it while it is being started.
func (m *Manager) lock(ctx context.Context, name string) (func(), error) {
	path := m.PIDFile(name)
	if err := fileutil.EnsureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return fileutil.Lock(ctx, path)
}

// removeStale removes a process id file that names no tunnel, so that the
// next status is honest and sshuttle, which refuses to start while the
// process a file names is alive, does not mistake another process for the
// tunnel.
func (m *Manager) removeStale(name string) error {
	if err := os.Remove(m.PIDFile(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Start brings a profile up. Starting one that is already running is
// reported rather than producing a second tunnel for the same networks.
func (m *Manager) Start(ctx context.Context, name string) error {
	args, err := m.Args(name)
	if err != nil {
		return err
	}
	unlock, err := m.lock(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	if _, running := m.running(name); running {
		return fmt.Errorf("tunnel %q is already running", name)
	}
	if err := m.removeStale(name); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// sshuttle asks for the local password when it needs to change the
	// firewall, so it keeps the terminal.
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("starting tunnel %q: %w", name, err)
	}
	return nil
}

// Stop brings a profile down. Only a configured profile is stopped, and only
// a process that is its tunnel is signalled.
func (m *Manager) Stop(ctx context.Context, name string) error {
	if _, err := m.Profile(name); err != nil {
		return err
	}
	unlock, err := m.lock(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	pid, running := m.running(name)
	if !running {
		if err := m.removeStale(name); err != nil {
			return err
		}
		if pid > 0 {
			return fmt.Errorf("tunnel %q is not running: process %d is not its sshuttle; removed the stale process id file", name, pid)
		}
		return fmt.Errorf("tunnel %q is not running", name)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("stopping tunnel %q: %w", name, err)
	}
	// sshuttle removes the file itself when it ends; this covers one that
	// is slow to.
	return m.removeStale(name)
}
