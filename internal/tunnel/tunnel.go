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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/tmpl"
)

// Manager starts, stops and reports on the tunnels of a site.
type Manager struct {
	// Profiles are the configured tunnels.
	Profiles map[string]v1alpha1.TunnelSpec
	// Networks maps a network name to a CIDR.
	Networks map[string]string
	// Host resolves a role name to the host sshuttle connects to.
	Host func(role string) (host, user string, err error)
	// StateDir holds the process id files.
	StateDir string
	// Binary is the sshuttle to run.
	Binary string
	// Vars are the template values a profile may refer to, such as
	// {workstation.host}.
	Vars map[string]string
}

// Names lists the configured profiles in sorted order.
func (m *Manager) Names() []string {
	out := make([]string, 0, len(m.Profiles))
	for name := range m.Profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Args builds the sshuttle argument vector for a profile.
func (m *Manager) Args(name string) ([]string, error) {
	profile, ok := m.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("unknown tunnel %q; the site defines %s", name, strings.Join(m.Names(), ", "))
	}
	if len(profile.Subnets) == 0 {
		return nil, fmt.Errorf("tunnel %q routes no subnet", name)
	}

	host, user := profile.Remote, profile.User
	if m.Host != nil {
		resolved, roleUser, err := m.Host(profile.Remote)
		if err != nil {
			return nil, fmt.Errorf("tunnel %q: %w", name, err)
		}
		host = resolved
		if user == "" {
			user = roleUser
		}
	}
	remote := host
	if user != "" {
		remote = user + "@" + host
	}

	binary := m.Binary
	if binary == "" {
		binary = "sshuttle"
	}
	args := []string{binary, "--daemon", "--pidfile", m.PIDFile(name), "--remote", remote}
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
			continue
		}
		args = append(args, "--exclude", value)
	}
	args = append(args, profile.Options...)
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

// running reads the process id file and checks that the process is alive, so
// that a stale file from a crash is not reported as a running tunnel.
func (m *Manager) running(name string) (int, bool) {
	data, err := os.ReadFile(m.PIDFile(name))
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
	return pid, true
}

// Start brings a profile up. Starting one that is already running is
// reported rather than producing a second tunnel for the same networks.
func (m *Manager) Start(ctx context.Context, name string) error {
	if _, running := m.running(name); running {
		return fmt.Errorf("tunnel %q is already running", name)
	}
	args, err := m.Args(name)
	if err != nil {
		return err
	}
	if err := fileutil.EnsureDir(filepath.Dir(m.PIDFile(name))); err != nil {
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

// Stop brings a profile down.
func (m *Manager) Stop(name string) error {
	pid, running := m.running(name)
	if !running {
		if pid > 0 {
			// The process is gone but the file is not; clean it up so the
			// next status is honest.
			_ = os.Remove(m.PIDFile(name))
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
	_ = os.Remove(m.PIDFile(name))
	return nil
}
