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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
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

// Format prints the backend without the password, whatever the verb, so
// that a log line or an error that prints one cannot give the account away.
func (b Backend) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "ipmi.Backend{Target: %q, Username: %q}", b.Target.String(), b.Username)
}

// Status is what one service processor answered.
type Status struct {
	// BMC is the service processor that was asked.
	BMC string `json:"bmc" yaml:"bmc"`
	// State is the power state it reported, "ok" for an action it
	// accepted, or "unknown" when it failed.
	State string `json:"state" yaml:"state"`
	// Err is set when the processor did not answer with success.
	Err string `json:"error,omitempty" yaml:"error,omitempty"`
	// Cause is the error behind Err. It carries the exit code: a
	// processor the backend never reported because the backend or the
	// connection to its host failed is a transport failure.
	Cause error `json:"-" yaml:"-"`
}

// Power runs a power action against a set of service processors, and
// returns one status for every processor of the set, in the order of the set.
//
// The whole set is handed to the backend in one call, because both tools take
// a host list and contacting a thousand processors one ssh connection at a
// time is what made the shell version unusable at scale.
//
// A processor counts as answered only when the backend printed an answer
// known to mean success for this action. Anything else it printed is the
// processor's error, and a processor it printed nothing for failed too,
// whatever the backend's exit code was.
func (b *Backend) Power(ctx context.Context, action string, bmcs *nodeset.NodeSet) ([]Status, error) {
	if bmcs == nil || bmcs.IsEmpty() {
		return nil, exitcode.Errorf(exitcode.Usage, "no service processor was selected")
	}
	if b.Password == "" {
		return nil, exitcode.Errorf(exitcode.Usage,
			"no password for the service processor account %q was resolved", b.Username)
	}
	if strings.ContainsAny(b.Username+b.Password, "\n\r\x00") {
		return nil, exitcode.Errorf(exitcode.Usage,
			"the service processor account %q has a line break or a NUL in its name or password, "+
				"which the IPMI tools cannot read", b.Username)
	}

	switch b.backend() {
	case BackendIpmitool:
		return b.runIpmitool(ctx, action, bmcs)
	default:
		return b.runIpmipower(ctx, action, bmcs)
	}
}

func (b *Backend) backend() string {
	return cmp.Or(b.Spec.Backend, BackendIpmipower)
}

// CheckAction reports whether the backends understand a power action.
func CheckAction(action string) error {
	if slices.Contains(Actions(), action) {
		return nil
	}
	return exitcode.Errorf(exitcode.Usage, "unknown power action %q; expected one of %s",
		action, strings.Join(Actions(), ", "))
}

// ipmipowerAction maps an action to the FreeIPMI flag.
func ipmipowerAction(action string) (string, error) {
	if err := CheckAction(action); err != nil {
		return "", err
	}
	switch action {
	case ActionStatus:
		return "--stat", nil
	case ActionOn:
		return "--on", nil
	case ActionOff:
		return "--off", nil
	case ActionCycle:
		return "--cycle", nil
	case ActionReset:
		return "--reset", nil
	default:
		return "--soft", nil
	}
}

// ipmitoolAction maps an action to the ipmitool subcommand.
func ipmitoolAction(action string) (string, error) {
	if err := CheckAction(action); err != nil {
		return "", err
	}
	return action, nil
}

// ipmipowerAnswer reads what ipmipower printed for one processor. It says
// "on" or "off" for a status and "ok" for an action it carried out; anything
// else is an error message.
func ipmipowerAnswer(action, text string) (string, bool) {
	answer := strings.ToLower(text)
	if action == ActionStatus {
		return answer, answer == "on" || answer == "off"
	}
	return "ok", answer == "ok"
}

// ipmitoolControl is what ipmitool prints when a power action was accepted.
var ipmitoolControl = map[string]string{
	ActionOn:    "Chassis Power Control: Up/On",
	ActionOff:   "Chassis Power Control: Down/Off",
	ActionCycle: "Chassis Power Control: Cycle",
	ActionReset: "Chassis Power Control: Reset",
	ActionSoft:  "Chassis Power Control: Soft",
}

// ipmitoolAnswer reads what ipmitool printed for one processor.
func ipmitoolAnswer(action, text string) (string, bool) {
	if action == ActionStatus {
		switch text {
		case "Chassis Power is on":
			return "on", true
		case "Chassis Power is off":
			return "off", true
		}
		return "", false
	}
	return "ok", text == ipmitoolControl[action]
}

// ipmipowerQuote writes a value for a FreeIPMI configuration file. The
// parser splits on white space and gives #, " and \ a meaning, and a # starts
// a comment even inside quotes, so the value is quoted with those escaped.
func ipmipowerQuote(value string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `#`, `\#`)
	return `"` + r.Replace(value) + `"`
}

func (b *Backend) runIpmipower(ctx context.Context, action string, bmcs *nodeset.NodeSet) ([]Status, error) {
	flag, err := ipmipowerAction(action)
	if err != nil {
		return nil, err
	}
	binary := cmp.Or(b.Spec.IpmipowerPath, "/usr/sbin/ipmipower")
	driver := cmp.Or(b.Spec.Driver, "LAN_2_0")

	argv := []string{binary, "--config-file", passwordFilePlaceholder,
		"--driver-type", driver, "--hostname", bmcs.Hostlist(), flag}

	// FreeIPMI reads the account out of a configuration file, so neither the
	// user nor the password reaches the argument vector.
	payload := fmt.Sprintf("username %s\npassword %s\n", ipmipowerQuote(b.Username), ipmipowerQuote(b.Password))
	result, err := b.run(ctx, argv, payload, b.Spec.Timeout.Get())
	if err != nil {
		return nil, err
	}
	return b.collect(result, bmcs, func(text string) (string, bool) { return ipmipowerAnswer(action, text) }), nil
}

// ipmitoolHostTimeout bounds one ipmitool run inside the loop. With one
// retry, an unreachable processor gives up after a few seconds.
const ipmitoolHostTimeout = 20 * time.Second

func (b *Backend) runIpmitool(ctx context.Context, action string, bmcs *nodeset.NodeSet) ([]Status, error) {
	sub, err := ipmitoolAction(action)
	if err != nil {
		return nil, err
	}
	binary := cmp.Or(b.Spec.IpmitoolPath, "/usr/bin/ipmitool")

	// ipmitool takes one host per invocation, so the loop runs on the
	// gateway rather than opening one ssh connection per processor. Each
	// run is bounded on its own and its exit code is printed with its last
	// line, so that one processor that hangs neither stops the others nor
	// passes for an answer.
	var script strings.Builder
	fmt.Fprintf(&script, "for h in %s; do\n", shellquote.Join(bmcs.Expand()))
	fmt.Fprintf(&script, "  out=$(timeout -k 5 %d %s -I lanplus -R 1 -U %s -f %s -H \"$h\" chassis power %s 2>&1) && rc=0 || rc=$?\n",
		int(ipmitoolHostTimeout/time.Second), shellquote.Quote(binary), shellquote.Quote(b.Username),
		passwordFilePlaceholder, shellquote.Quote(sub))
	script.WriteString("  last=$(printf '%s\\n' \"$out\" | tail -n1)\n")
	script.WriteString("  if [ \"$rc\" -eq 0 ]; then printf '%s: %s\\n' \"$h\" \"$last\"; " +
		"else printf '%s: exit %s: %s\\n' \"$h\" \"$rc\" \"$last\"; fi\n")
	script.WriteString("done\n")

	// The whole loop may take every run's bound, and the configured timeout
	// on top for the connection.
	timeout := b.Spec.Timeout.Get() + time.Duration(bmcs.Len())*(ipmitoolHostTimeout+5*time.Second)
	result, err := b.runScript(ctx, script.String(), b.Password+"\n", timeout)
	if err != nil {
		return nil, err
	}
	return b.collect(result, bmcs, func(text string) (string, bool) {
		if strings.HasPrefix(text, "exit ") {
			return "", false
		}
		return ipmitoolAnswer(action, text)
	}), nil
}

// collect reads the answer of every processor of the set out of what the
// backend printed, and accounts for the ones it did not answer for.
func (b *Backend) collect(result *transport.Result, bmcs *nodeset.NodeSet, answer func(string) (string, bool)) []Status {
	lines := strings.Split(result.Stdout+"\n"+result.Stderr, "\n")
	names := bmcs.Expand()
	statuses := make([]Status, 0, len(names))
	for _, bmc := range names {
		text, found := "", false
		// A line belongs to a processor when it starts with its whole name
		// and a colon, so that bmc1 is not answered for by bmc10.
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(line, bmc+":"); ok {
				text, found = strings.TrimSpace(rest), true
			}
		}
		status := Status{BMC: bmc}
		switch state, ok := answer(text); {
		case found && ok:
			status.State = state
		case found:
			status.State = "unknown"
			status.Err = text
			status.Err = cmp.Or(status.Err, "the IPMI backend printed nothing for it")
			status.Cause = errors.New(status.Err)
		default:
			status.State = "unknown"
			status.Cause = b.unreported(result)
			status.Err = status.Cause.Error()
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// unreported explains a processor the backend printed no answer for. When
// the backend or the connection to its host failed, the processor was not
// reached, which is a transport failure; an action may or may not have been
// carried out.
func (b *Backend) unreported(result *transport.Result) error {
	const what = "not reported by the IPMI backend"
	switch {
	case result.Err != nil:
		return fmt.Errorf("%s, which failed: %w", what, result.Err)
	case result.ExitCode == 124 || result.ExitCode == 137:
		return exitcode.Errorf(exitcode.Transport,
			"%s, which was stopped at its timeout (exit %d); check the power state", what, result.ExitCode)
	case result.ExitCode != 0:
		detail := ""
		if line := lastLine(result.Stderr); line != "" {
			detail = ": " + line
		}
		return exitcode.Errorf(exitcode.Transport, "%s, which exited %d%s", what, result.ExitCode, detail)
	default:
		return errors.New(what)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
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
func (b *Backend) run(ctx context.Context, argv []string, payload string, timeout time.Duration) (*transport.Result, error) {
	command := shellquote.Join(argv)
	command = strings.ReplaceAll(command, shellquote.Quote(passwordFilePlaceholder), `"$secret"`)
	command = strings.ReplaceAll(command, passwordFilePlaceholder, `"$secret"`)
	return b.runScript(ctx, command, payload, timeout)
}

// runScript sends a script to the backend host with the secret on stdin. It
// fails only when the script could not be sent at all; what the backend
// said, and how it exited, is for the caller to read.
func (b *Backend) runScript(ctx context.Context, body, payload string, timeout time.Duration) (*transport.Result, error) {
	body = strings.ReplaceAll(body, passwordFilePlaceholder, `"$secret"`)
	script := fmt.Sprintf(wrapper, body)

	return b.Runner.Run(ctx, b.Target, transport.Request{
		Script:  script,
		Stdin:   strings.NewReader(payload),
		Timeout: timeout,
		TTY:     transport.TTYNone,
	})
}
