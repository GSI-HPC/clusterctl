// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package ipmi drives the FreeIPMI and ipmitool backends that talk to service
// processors over the management network.
//
// The tools run on the host that has a route into that network, usually the
// management gateway, and clusterctl reaches them over ssh. The password
// never appears in an argument vector: it is streamed over stdin into a file
// the backend reads and that is removed again, because ps on a shared gateway
// shows every argument to every user.
package ipmi

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// The power actions the backends understand.
const (
	ActionStatus = "status"
	ActionOn     = "on"
	ActionOff    = "off"
	ActionCycle  = "cycle"
	ActionReset  = "reset"
	// ActionSoft asks the operating system to shut down, which ipmitool
	// calls soft and FreeIPMI calls soft-shutdown.
	ActionSoft = "soft"
)

// Actions lists the power actions, for help text and completion.
func Actions() []string {
	return []string{ActionStatus, ActionOn, ActionOff, ActionCycle, ActionReset, ActionSoft}
}

// Backends names the supported backends.
const (
	BackendIpmipower = "ipmipower"
	BackendIpmitool  = "ipmitool"
)

// Backend runs an IPMI tool on a host that can reach the service network.
type Backend struct {
	// Runner executes the command, usually over ssh.
	Runner transport.Runner
	// Target is the host the tool runs on.
	Target transport.Target
	// Spec is the IPMI configuration of the site.
	Spec v1alpha1.IPMISpec
	// Username and Password authenticate with the service processors.
	Username string
	Password string
}

// Status is what one service processor answered.
type Status struct {
	// BMC is the service processor that was asked.
	BMC string `json:"bmc" yaml:"bmc"`
	// State is the power state it reported, or the outcome of the action.
	State string `json:"state" yaml:"state"`
	// Err is set when the tool reported a problem for this host.
	Err string `json:"error,omitempty" yaml:"error,omitempty"`
}

// Power runs a power action against a set of service processors.
//
// The whole set is handed to the backend in one call, because both tools take
// a host list and contacting a thousand processors one ssh connection at a
// time is what made the shell version unusable at scale.
func (b *Backend) Power(ctx context.Context, action string, bmcs *nodeset.NodeSet) ([]Status, error) {
	if bmcs == nil || bmcs.IsEmpty() {
		return nil, fmt.Errorf("no service processor was selected")
	}
	if b.Password == "" {
		return nil, fmt.Errorf("no password for the service processor account %q was resolved", b.Username)
	}

	switch b.backend() {
	case BackendIpmitool:
		return b.runIpmitool(ctx, action, bmcs)
	default:
		return b.runIpmipower(ctx, action, bmcs)
	}
}

func (b *Backend) backend() string {
	if b.Spec.Backend != "" {
		return b.Spec.Backend
	}
	return BackendIpmipower
}

// ipmipowerAction maps an action to the FreeIPMI flag.
func ipmipowerAction(action string) (string, error) {
	switch action {
	case ActionStatus, "":
		return "--stat", nil
	case ActionOn:
		return "--on", nil
	case ActionOff:
		return "--off", nil
	case ActionCycle:
		return "--cycle", nil
	case ActionReset:
		return "--reset", nil
	case ActionSoft:
		return "--soft", nil
	default:
		return "", fmt.Errorf("unknown power action %q; expected one of %s", action, strings.Join(Actions(), ", "))
	}
}

// ipmitoolAction maps an action to the ipmitool subcommand.
func ipmitoolAction(action string) (string, error) {
	switch action {
	case ActionStatus, "":
		return "status", nil
	case ActionOn, ActionOff, ActionCycle, ActionReset, ActionSoft:
		return action, nil
	default:
		return "", fmt.Errorf("unknown power action %q; expected one of %s", action, strings.Join(Actions(), ", "))
	}
}

func (b *Backend) runIpmipower(ctx context.Context, action string, bmcs *nodeset.NodeSet) ([]Status, error) {
	flag, err := ipmipowerAction(action)
	if err != nil {
		return nil, err
	}
	binary := b.Spec.IpmipowerPath
	if binary == "" {
		binary = "/usr/sbin/ipmipower"
	}
	driver := b.Spec.Driver
	if driver == "" {
		driver = "LAN_2_0"
	}

	argv := []string{binary, "--config-file", passwordFilePlaceholder,
		"--driver-type", driver, "--hostname", bmcs.Hostlist(), flag}

	// FreeIPMI reads the account out of a configuration file, so neither the
	// user nor the password reaches the argument vector.
	payload := fmt.Sprintf("username %s\npassword %s\n", b.Username, b.Password)
	result, err := b.run(ctx, argv, payload)
	if err != nil {
		return nil, err
	}
	return parseColonStatus(result.Stdout + result.Stderr), nil
}

func (b *Backend) runIpmitool(ctx context.Context, action string, bmcs *nodeset.NodeSet) ([]Status, error) {
	sub, err := ipmitoolAction(action)
	if err != nil {
		return nil, err
	}
	binary := b.Spec.IpmitoolPath
	if binary == "" {
		binary = "/usr/bin/ipmitool"
	}

	// ipmitool takes one host per invocation, so the loop runs on the
	// gateway rather than opening one ssh connection per processor.
	var b2 strings.Builder
	fmt.Fprintf(&b2, "for h in %s; do\n", shellquote.Join(bmcs.Expand()))
	fmt.Fprintf(&b2, "  printf '%%s: ' \"$h\"\n")
	fmt.Fprintf(&b2, "  %s -I lanplus -R 1 -U %s -f %s -H \"$h\" chassis power %s 2>&1 | tail -n1\n",
		shellquote.Quote(binary), shellquote.Quote(b.Username), passwordFilePlaceholder, shellquote.Quote(sub))
	b2.WriteString("done\n")

	result, err := b.runScript(ctx, b2.String(), b.Password+"\n")
	if err != nil {
		return nil, err
	}
	return parseColonStatus(result.Stdout + result.Stderr), nil
}

// passwordFilePlaceholder is replaced by the private file the wrapper writes
// the secret into.
const passwordFilePlaceholder = "@CLUSTERCTL_PASSWORD_FILE@"

// wrapper writes what arrives on standard input into a file only its owner
// can read, runs the command with the placeholder replaced by that file's
// name, and removes it again whatever happens.
const wrapper = `
set -eu
umask 077
secret=$(mktemp "${TMPDIR:-/tmp}/clusterctl.XXXXXXXX")
trap 'rm -f "$secret"' EXIT INT TERM
cat > "$secret"
%s
`

// run sends a command to the backend host with the secret on stdin.
func (b *Backend) run(ctx context.Context, argv []string, payload string) (*transport.Result, error) {
	command := shellquote.Join(argv)
	command = strings.ReplaceAll(command, shellquote.Quote(passwordFilePlaceholder), `"$secret"`)
	command = strings.ReplaceAll(command, passwordFilePlaceholder, `"$secret"`)
	return b.runScript(ctx, command, payload)
}

// runScript sends a script to the backend host with the secret on stdin.
func (b *Backend) runScript(ctx context.Context, body, payload string) (*transport.Result, error) {
	body = strings.ReplaceAll(body, passwordFilePlaceholder, `"$secret"`)
	script := fmt.Sprintf(wrapper, body)

	result, err := b.Runner.Run(ctx, b.Target, transport.Request{
		Script:  script,
		Stdin:   strings.NewReader(payload),
		Timeout: b.Spec.Timeout.Get(),
		TTY:     transport.TTYNone,
	})
	if err != nil {
		return nil, err
	}
	if result.Failed() && strings.TrimSpace(result.Stdout) == "" {
		if result.Err != nil {
			return nil, result.Err
		}
		return nil, fmt.Errorf("%s: the IPMI backend exited %d: %s",
			b.Target, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

// parseColonStatus reads the "host: state" lines both tools produce.
func parseColonStatus(out string) []Status {
	var statuses []Status
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host, state, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		state = strings.TrimSpace(state)
		status := Status{BMC: strings.TrimSpace(host), State: state}
		if isFailure(state) {
			status.Err = state
			status.State = "unknown"
		}
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].BMC < statuses[j].BMC })
	return statuses
}

// isFailure recognises the answers the tools give when they could not talk to
// a processor.
func isFailure(state string) bool {
	lowered := strings.ToLower(state)
	for _, marker := range []string{
		"error", "timeout", "timed out", "unable", "could not",
		"connection", "authentication", "privilege", "unknown host", "invalid",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}
