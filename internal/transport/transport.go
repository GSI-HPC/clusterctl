// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package transport runs commands on remote hosts through the OpenSSH
// client.
//
// OpenSSH is driven rather than replaced, so ProxyJump, ControlMaster,
// Kerberos, the agent and the administrator's own configuration keep working
// exactly as they do from a shell.
//
// A remote command is never built by joining words into a string. The
// argument vector is quoted once, handed to ssh as a single argument, and
// split back by the remote shell into exactly the arguments that were given,
// so a glob, an apostrophe or repeated whitespace survives the trip.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/hostname"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
)

// Defaults used when the configuration leaves a value unset.
const (
	defaultConnectTimeout = 10 * time.Second
	defaultShell          = "bash"
	// killGrace is how long a command gets after its timeout before it is
	// killed outright.
	killGrace = 5 * time.Second
	// maxArgBytes is the size of one argument the kernel accepts. A remote
	// command longer than this has to travel over stdin instead.
	maxArgBytes = 131072
	// sshConnectionFailed is the exit status ssh itself reports when it
	// could not reach or authenticate with the host.
	sshConnectionFailed = 255
)

// TTY says whether a pseudo terminal is allocated for a remote command.
type TTY int

const (
	// TTYAuto allocates a terminal for an interactive login and none for a
	// command, which is what ssh does on its own.
	TTYAuto TTY = iota
	// TTYForce allocates one even for a command, for anything that prompts.
	TTYForce
	// TTYNone never allocates one, so that output is not echoed back.
	TTYNone
)

// Target is a host to run something on.
type Target struct {
	// Name is the node or role as the administrator named it, used in
	// output and errors.
	Name string `json:"name" yaml:"name"`
	// Host is the name ssh connects to.
	Host string `json:"host" yaml:"host"`
	// User is the remote account; empty uses what the configuration says.
	User string `json:"user" yaml:"user"`
	// Role names the infrastructure role, when the target is one.
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	// ForwardAgent and ForwardX11 are requested per command, on top of what
	// the role already asks for.
	ForwardAgent bool `json:"forwardAgent,omitempty" yaml:"forwardAgent,omitempty"`
	ForwardX11   bool `json:"forwardX11,omitempty" yaml:"forwardX11,omitempty"`
}

// String renders the target the way errors refer to it.
func (t Target) String() string {
	if t.Name != "" && t.Name != t.Host {
		return fmt.Sprintf("%s (%s)", t.Name, t.Host)
	}
	return t.Host
}

// Request is what to run on a target.
type Request struct {
	// Argv is the remote command. An empty Argv and Script start an
	// interactive login.
	Argv []string
	// Script is a shell program run with the shell's -c, as one argument.
	// Argv and Script are mutually exclusive.
	Script string
	// Shell runs Script; it defaults to bash.
	Shell string
	// TTY controls pseudo terminal allocation.
	TTY TTY
	// Timeout bounds the command. It is enforced on the target with
	// timeout(1), because killing the local ssh leaves the remote process
	// running.
	Timeout time.Duration
	// Env is exported for the command. It is passed through env(1) rather
	// than SendEnv, which needs the server to allow each name.
	Env map[string]string
	// Stdin is fed to the command. Secrets travel this way so that they
	// never appear in an argument vector, where ps shows them.
	Stdin io.Reader
	// Stdout and Stderr receive the output when the command is run
	// interactively. Run captures them instead.
	Stdout, Stderr io.Writer
}

// Result is the outcome of one remote command.
type Result struct {
	Target   Target        `json:"target" yaml:"target"`
	ExitCode int           `json:"exitCode" yaml:"exitCode"`
	Stdout   string        `json:"stdout,omitempty" yaml:"stdout,omitempty"`
	Stderr   string        `json:"stderr,omitempty" yaml:"stderr,omitempty"`
	Duration time.Duration `json:"-" yaml:"-"`
	// Err is set when the command could not be run or did not exit zero.
	Err error `json:"-" yaml:"-"`
}

// MarshalJSON adds the error text to the result, which the machine formats
// would otherwise leave out: Err is an error value, not a string.
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	out := struct {
		plain
		Error string `json:"error,omitempty"`
	}{plain: plain(r)}
	if r.Err != nil {
		out.Error = r.Err.Error()
	}
	return json.Marshal(out)
}

// Failed reports whether the command did not succeed.
func (r *Result) Failed() bool { return r == nil || r.Err != nil || r.ExitCode != 0 }

// Output returns the standard output with trailing newlines removed.
func (r *Result) Output() string { return strings.TrimRight(r.Stdout, "\n") }

// Lines returns the standard output split into non-empty lines.
func (r *Result) Lines() []string {
	var out []string
	for _, line := range strings.Split(r.Stdout, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Runner runs a command on a target. Commands depend on this rather than on
// the client, so that a dry run or a test can stand in for real ssh.
type Runner interface {
	Run(ctx context.Context, target Target, req Request) (*Result, error)
}

// Client drives the OpenSSH client.
type Client struct {
	spec        v1alpha1.SSHSpec
	roles       map[string]v1alpha1.HostRole
	stateDir    string
	knownHosts  string
	defaultUser string

	once       sync.Once
	configPath string
	configErr  error
}

// Options configure a client.
type Options struct {
	// SSH is the transport configuration of the site.
	SSH v1alpha1.SSHSpec
	// Roles are the infrastructure roles, used for per role settings and
	// for resolving a jump host by name.
	Roles map[string]v1alpha1.HostRole
	// StateDir holds the generated configuration and the control sockets.
	StateDir string
	// KnownHostsFile is the host key file, already made absolute.
	KnownHostsFile string
	// DefaultUser is the account used where nothing names one.
	DefaultUser string
}

// New returns a client. The ssh configuration is written on first use.
func New(opts Options) *Client {
	return &Client{
		spec:        opts.SSH,
		roles:       opts.Roles,
		stateDir:    opts.StateDir,
		knownHosts:  opts.KnownHostsFile,
		defaultUser: opts.DefaultUser,
	}
}

// ConfigPath returns the generated ssh configuration, writing it once.
func (c *Client) ConfigPath() (string, error) {
	c.once.Do(func() { c.configPath, c.configErr = c.writeConfig() })
	return c.configPath, c.configErr
}

// Binary returns the ssh client to run.
func (c *Client) Binary() string {
	if c.spec.Binary != "" {
		return c.spec.Binary
	}
	return "ssh"
}

// ScpBinary returns the scp client to run.
func (c *Client) ScpBinary() string {
	if c.spec.ScpBinary != "" {
		return c.spec.ScpBinary
	}
	return "scp"
}

// Command returns ssh with the generated configuration, the part of every
// argument vector that decides how a host is reached and which host key it
// must present. A program that runs ssh itself, such as sshuttle, is given
// this command, so that it connects the way every other connection does.
func (c *Client) Command() ([]string, error) {
	config, err := c.ConfigPath()
	if err != nil {
		return nil, err
	}
	return []string{c.Binary(), "-F", config}, nil
}

// Destination renders the [user@]host a target is reached as, with the
// account resolved and checked as it is for a command.
func (c *Client) Destination(target Target) (string, error) {
	return c.destination(target)
}

// Args builds the full argument vector, ssh included, that a request runs as.
// Commands use it for --dry-run and for reporting what they would do.
func (c *Client) Args(target Target, req Request) ([]string, error) {
	args, err := c.Command()
	if err != nil {
		return nil, err
	}

	switch req.TTY {
	case TTYForce:
		args = append(args, "-tt")
	case TTYNone:
		args = append(args, "-T")
	}
	if target.ForwardAgent {
		args = append(args, "-A")
	}
	if target.ForwardX11 {
		args = append(args, "-X")
	}

	dest, err := c.destination(target)
	if err != nil {
		return nil, err
	}
	// The destination follows "--", so that nothing in it can be read as an
	// option, as CopyArgs does for scp.
	args = append(args, "--", dest)

	command, err := RemoteCommand(req)
	if err != nil {
		return nil, err
	}
	if command != "" {
		args = append(args, command)
	}
	return args, nil
}

// destination renders the [user@]host ssh connects to, refusing a host or an
// account that is not spelled as one. Either may come from a node set that an
// agent or a group source supplied.
func (c *Client) destination(target Target) (string, error) {
	if err := hostname.CheckHost(target.Host); err != nil {
		return "", exitcode.Wrap(exitcode.Usage, fmt.Errorf("%s: %w", target.Name, err))
	}
	user := c.userFor(target)
	if user == "" {
		return target.Host, nil
	}
	if !isUserName(user) {
		return "", exitcode.Errorf(exitcode.Usage,
			"%s: %q is not a user name: use letters, digits, '.', '_' and '-', not beginning with '-'",
			target.Name, user)
	}
	return user + "@" + target.Host, nil
}

// isUserName reports whether an account is spelled in the portable user name
// alphabet of POSIX, in which ssh gives no character a meaning.
func isUserName(user string) bool {
	if user == "" || user[0] == '-' {
		return false
	}
	for i := 0; i < len(user); i++ {
		if !isUserChar(user[i]) {
			return false
		}
	}
	return true
}

func isUserChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-'
}

// userFor resolves the account to log in as.
func (c *Client) userFor(target Target) string {
	if target.User != "" {
		return target.User
	}
	if role, ok := c.roles[target.Role]; ok && role.User != "" {
		return role.User
	}
	return c.defaultUser
}

// RemoteCommand renders a request as the single argument ssh is given.
//
// The result is one shell command line. The remote shell splits it back into
// the original argument vector, because every word was quoted.
func RemoteCommand(req Request) (string, error) {
	if len(req.Argv) > 0 && req.Script != "" {
		return "", errors.New("a request carries both an argument vector and a script")
	}

	var parts []string
	if req.Timeout > 0 {
		parts = append(parts, "timeout", "-k",
			duration(killGrace), duration(req.Timeout))
	}
	if len(req.Env) > 0 {
		parts = append(parts, "env")
		for _, k := range sortedKeys(req.Env) {
			parts = append(parts, k+"="+req.Env[k])
		}
	}

	switch {
	case req.Script != "":
		shell := req.Shell
		if shell == "" {
			shell = defaultShell
		}
		parts = append(parts, shell, "-c", req.Script)
	case len(req.Argv) > 0:
		parts = append(parts, req.Argv...)
	case len(parts) > 0:
		return "", errors.New("a timeout or environment was given without a command")
	default:
		return "", nil
	}

	command := shellquote.Join(parts)
	if len(command) > maxArgBytes {
		return "", fmt.Errorf(
			"the remote command is %d bytes, over the %d byte limit on one argument; send it over stdin instead",
			len(command), maxArgBytes)
	}
	return command, nil
}

// duration renders a duration the way timeout(1) reads it.
func duration(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%.3fs", d.Seconds())
}

// Run executes a request and captures its output.
func (c *Client) Run(ctx context.Context, target Target, req Request) (*Result, error) {
	args, err := c.Args(target, req)
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	cmd := command(ctx, args)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = req.Stdin
	if cmd.Stdin == nil {
		// Without this ssh inherits the terminal and a remote command may
		// consume the keystrokes meant for clusterctl itself.
		cmd.Stdin = bytes.NewReader(nil)
	}

	start := time.Now()
	runErr := cmd.Run()
	result := &Result{
		Target:   target,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}
	result.ExitCode, result.Err = classify(ctx, target, runErr, result.Stderr)
	return result, nil
}

// Interactive runs a request wired to the terminal, for a login shell or
// anything that needs to talk to the user.
func (c *Client) Interactive(ctx context.Context, target Target, req Request) error {
	args, err := c.Args(target, req)
	if err != nil {
		return err
	}
	cmd := command(ctx, args)
	cmd.Stdin = req.Stdin
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = req.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = req.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	_, err = classify(ctx, target, cmd.Run(), "")
	return err
}

// command prepares ssh or scp to run until ctx ends.
//
// When ctx ends, the client is sent SIGTERM rather than killed, so that it
// closes the session and puts the terminal back as it found it; it is killed
// only if it has not gone killGrace later. Wait gives up on the output at the
// same point, since a process the client started, such as a ProxyCommand,
// can hold it open after the client has gone.
//
// Stopping the client does not stop the remote command: without a terminal
// on the host nothing tells it, and it runs until its own timeout ends it or
// it fails to write.
func command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killGrace
	return cmd
}

// classify turns the outcome of running ssh into an exit code and an error
// that carries the right code for the process.
//
// A command that ended because its context did is reported as interrupted,
// whatever ssh exited with: once ssh has been killed, its status says nothing
// about the host.
func classify(ctx context.Context, target Target, err error, stderr string) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	isExit := errors.As(err, &exitErr)
	if ctxErr := ctx.Err(); ctxErr != nil {
		code := -1
		if isExit {
			code = exitErr.ExitCode()
		}
		return code, cancelled(target, ctxErr)
	}
	if !isExit {
		return -1, exitcode.Wrap(exitcode.Transport,
			fmt.Errorf("running ssh for %s: %w", target, err))
	}
	code := exitErr.ExitCode()
	return code, exited(target, code, stderr)
}

// cancelled is the error of a command that was stopped because its context
// ended. An interrupt exits 130; a deadline is the command's failure.
func cancelled(target Target, err error) error {
	code := exitcode.TargetFailed
	if errors.Is(err, context.Canceled) {
		code = exitcode.Interrupted
	}
	return exitcode.Wrap(code, fmt.Errorf("%s: %w", target, err))
}

// exited is the error of a command that ran and exited with a status. ssh's
// own status 255 means it could not reach or authenticate with the host;
// anything else came from the host, which answered.
func exited(target Target, code int, stderr string) error {
	switch code {
	case 0:
		return nil
	case sshConnectionFailed:
		detail := strings.TrimSpace(lastLine(stderr))
		if detail == "" {
			detail = "ssh reported a connection failure"
		}
		return exitcode.Wrap(exitcode.Transport, fmt.Errorf("%s: %s", target, detail))
	default:
		return fmt.Errorf("%s: command exited %d", target, code)
	}
}

// ExitResult is the result Run reports for a command that ran and exited
// with a status, with its error set the same way. A stand-in for the client,
// such as a test's, uses it so that a failure looks as it would over ssh.
func ExitResult(target Target, code int, stdout, stderr string) *Result {
	return &Result{
		Target:   target,
		ExitCode: code,
		Stdout:   stdout,
		Stderr:   stderr,
		Err:      exited(target, code, stderr),
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
