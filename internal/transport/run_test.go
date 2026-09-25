// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// fakeClient returns a client whose ssh and scp are a shell script, so that
// Run and Copy can be driven through a real process without a host.
func fakeClient(t *testing.T, script string) *transport.Client {
	t.Helper()
	return fakeClientWith(t, script, transport.Options{})
}

// fakeClientWith is fakeClient with further options.
func fakeClientWith(t *testing.T, script string, opts transport.Options) *transport.Client {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-ssh")
	// ssh -V is asked for the version before the configuration is written,
	// and is answered at once, whatever the script does otherwise.
	version := "[ \"$1\" = -V ] && { echo OpenSSH_9.6p1 >&2; exit 0; }\n"
	writeScript(t, binary, "#!/bin/sh\n"+version+script+"\n")
	opts.SSH.Binary, opts.SSH.ScpBinary = binary, binary
	opts.StateDir = dir
	opts.KnownHostsFile = filepath.Join(dir, "ssh-known-hosts")
	return transport.New(opts)
}

// writeScript writes an executable script from a child process. Written
// from this one, the file would be open for writing while a parallel test
// forks, the child would inherit that descriptor until it execs, and running
// the script meanwhile fails with "text file busy".
func writeScript(t *testing.T, path, content string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `cat > "$1" && chmod 700 "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writing %s: %v: %s", path, err, out)
	}
}

var target = transport.Target{Name: "exe0001", Host: "exe0001.example.org"}

// cancelSoon returns a context that is cancelled shortly after the command
// has started, the way the first Ctrl-C cancels the process's context.
func cancelSoon(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	time.AfterFunc(200*time.Millisecond, cancel)
	return ctx
}

// TestRunReportsAnInterruptAsInterrupted checks that a command stopped by its
// context says so. The killed ssh used to be reported as "command exited -1",
// an error without a code, so an interrupted slurm node drain exited 1 and an
// interrupted boot set 3.
func TestRunReportsAnInterruptAsInterrupted(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, "exec sleep 30")
	start := time.Now()
	result, err := c.Run(cancelSoon(t), target, transport.Request{Argv: []string{"true"}, TTY: transport.TTYNone})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", result.Err)
	}
	if got := exitcode.From(result.Err); got != exitcode.Interrupted {
		t.Errorf("exit code = %d, want %d", got, exitcode.Interrupted)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Run took %v to return after the interrupt", elapsed)
	}
}

func TestInteractiveReportsAnInterruptAsInterrupted(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, "exec sleep 30")
	err := c.Interactive(cancelSoon(t), target, transport.Request{
		Argv: []string{"true"}, Stdin: strings.NewReader(""), Stdout: &strings.Builder{}, Stderr: &strings.Builder{},
	})
	if !errors.Is(err, context.Canceled) || exitcode.From(err) != exitcode.Interrupted {
		t.Errorf("error = %v (exit code %d), want an interrupt", err, exitcode.From(err))
	}
}

func TestCopyReportsAnInterruptAsInterrupted(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, "exec sleep 30")
	result, err := c.Copy(cancelSoon(t), target, transport.CopyRequest{
		Sources: []string{"/etc/hosts"}, Destination: "/tmp/hosts", Upload: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(result.Err, context.Canceled) || exitcode.From(result.Err) != exitcode.Interrupted {
		t.Errorf("error = %v (exit code %d), want an interrupt", result.Err, exitcode.From(result.Err))
	}
}

// scp draws its progress meter on its standard output, which was the
// process's standard error whoever asked for the transfer. It is the
// request's Progress now, and the null device without one, where scp draws
// nothing.
func TestCopySendsScpsOutputToProgressOnly(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, `[ /dev/stdout -ef /dev/null ] && echo discarded >&2; echo "hosts 100%"`)
	req := transport.CopyRequest{Sources: []string{"/etc/hosts"}, Destination: "/tmp/hosts", Upload: true}

	var progress strings.Builder
	req.Progress = &progress
	result, err := c.Copy(context.Background(), target, req)
	if err != nil || result.Err != nil {
		t.Fatalf("copy failed: %v %v", err, result.Err)
	}
	if got, want := progress.String(), "hosts 100%\n"; got != want {
		t.Errorf("Progress got %q, want %q", got, want)
	}

	req.Progress = nil
	result, err = c.Copy(context.Background(), target, req)
	if err != nil || result.Err != nil {
		t.Fatalf("copy failed: %v %v", err, result.Err)
	}
	if !strings.Contains(result.Stderr, "discarded") {
		t.Errorf("without Progress, scp's output did not go to the null device (stderr %q)", result.Stderr)
	}
}

// ssh is asked for its version before the configuration is written, and it
// was asked under a context of its own, so an interrupt did not reach it: a
// client that hung there held the command for five seconds, and the
// configuration was then written from a guess. The question stops with the
// command now, and an interrupted one writes nothing.
func TestTheVersionQuestionStopsWithTheCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "hanging-ssh")
	writeScript(t, binary, "#!/bin/sh\nexec sleep 30\n")
	stateDir := filepath.Join(dir, "state")
	c := transport.New(transport.Options{
		SSH:            v1alpha1.SSHSpec{Binary: binary},
		StateDir:       stateDir,
		KnownHostsFile: filepath.Join(dir, "ssh-known-hosts"),
		Context:        cancelSoon(t),
	})

	start := time.Now()
	_, err := c.ConfigPath()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the configuration took %v to give up after the interrupt", elapsed)
	}
	if !errors.Is(err, context.Canceled) || exitcode.From(err) != exitcode.Interrupted {
		t.Errorf("error = %v (exit code %d), want an interrupt", err, exitcode.From(err))
	}
	if written, _ := filepath.Glob(filepath.Join(stateDir, "ssh_config-*")); len(written) > 0 {
		t.Errorf("a configuration was written although the version was never told: %v", written)
	}
}

// TestRunBoundsACommandWithATimeout checks that a command with a timeout is
// bounded here as well as by timeout(1) on the host. A host that stopped
// answering held Run until ssh's keepalives gave up, three minutes with the
// defaults. ssh is stopped once the command has had its timeout, the kill
// grace and the time reaching the host may take, which leaves a host that
// answers late its own answer, and a host that did not is reported as
// unreachable, not as interrupted.
func TestRunBoundsACommandWithATimeout(t *testing.T) {
	t.Parallel()
	background := func(*testing.T) context.Context { return context.Background() }
	for _, tc := range []struct {
		name    string
		script  string
		ctx     func(*testing.T) context.Context
		code    int
		detail  string
		atLeast time.Duration
	}{
		{"a host that stopped answering", "exec sleep 60", background,
			exitcode.Transport, "no answer 6s after the command's 100ms timeout ran out", 6100 * time.Millisecond},
		{"a host that answered after its timeout", "sleep 1; exit 124", background,
			exitcode.TargetFailed, "command exited 124", time.Second},
		{"an interrupt", "exec sleep 60", cancelSoon,
			exitcode.Interrupted, "context canceled", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// One attempt of one second: reaching the host may take a
			// second, the least the generated configuration can say.
			c := fakeClientWith(t, tc.script, transport.Options{SSH: v1alpha1.SSHSpec{
				ConnectTimeout: v1alpha1.Duration(100 * time.Millisecond), ConnectionAttempts: 1}})
			start := time.Now()
			result, err := c.Run(tc.ctx(t), target, transport.Request{Argv: []string{"true"}, Timeout: 100 * time.Millisecond})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			if got := exitcode.From(result.Err); got != tc.code {
				t.Errorf("exit code = %d, want %d (error %v)", got, tc.code, result.Err)
			}
			if result.Err == nil || !strings.Contains(result.Err.Error(), tc.detail) {
				t.Errorf("error = %v, want it to say %q", result.Err, tc.detail)
			}
			if tc.code != exitcode.Interrupted && errors.Is(result.Err, context.Canceled) {
				t.Errorf("error = %v, which reads as an interrupt", result.Err)
			}
			if elapsed < tc.atLeast || elapsed > tc.atLeast+10*time.Second {
				t.Errorf("Run returned after %v, want %v or a little more", elapsed, tc.atLeast)
			}
		})
	}
}

// The time reaching a host may take is what the generated configuration
// lets ssh spend: every attempt of ssh.connectTimeout, in the whole seconds
// the file holds, with a second between attempts, and as much again for
// each jump host ssh connects to first. A bound that left the jump hosts
// out would stop ssh while a command behind a slow gateway still ran, and
// one that left out what a role's options set would stop it while ssh
// still spent the time they give.
func TestTheTimeToReachAHostCountsEveryAttemptAndJump(t *testing.T) {
	t.Parallel()
	roles := map[string]v1alpha1.HostRole{
		"mgmt":  {Host: "mgmt.example.org"},
		"inner": {Host: "inner.example.org", ProxyJump: "mgmt"},
		"dhcp":  {Host: "dhcp.example.org", ProxyJump: "mgmt"},
		// ssh reaches inner with inner's own block, and gw2 with the rest
		// of this chain rather than its own.
		"deep": {Host: "deep.example.org", ProxyJump: "inner,gw2"},
		"gw2":  {Host: "gw2.example.org", ProxyJump: "mgmt"},
		// A role's own options come first in the file, and win.
		"pdu":     {Host: "pdu.example.org", Options: map[string]string{"ConnectTimeout": "60", "connectionAttempts": "3"}},
		"behind":  {Host: "behind.example.org", ProxyJump: "pdu"},
		"proxied": {Host: "proxied.example.org", Options: map[string]string{"ProxyCommand": "nc -X connect -x proxy:3128 %h %p"}},
	}
	for _, tc := range []struct {
		name string
		ssh  v1alpha1.SSHSpec
		host string
		want time.Duration
	}{
		{"the defaults, directly", v1alpha1.SSHSpec{}, "exe0001.example.org", 10 * time.Second},
		{"two attempts a second apart", v1alpha1.SSHSpec{ConnectTimeout: v1alpha1.Duration(10 * time.Second), ConnectionAttempts: 2},
			"exe0001.example.org", 21 * time.Second},
		{"a timeout in whole seconds", v1alpha1.SSHSpec{ConnectTimeout: v1alpha1.Duration(1500 * time.Millisecond)},
			"exe0001.example.org", 2 * time.Second},
		{"through a jump host", v1alpha1.SSHSpec{ConnectTimeout: v1alpha1.Duration(10 * time.Second), ConnectionAttempts: 2},
			"dhcp.example.org", 42 * time.Second},
		{"through a chain whose first hop has a jump host of its own", v1alpha1.SSHSpec{ConnectTimeout: v1alpha1.Duration(5 * time.Second)},
			"deep.example.org", 20 * time.Second},
		{"a role's own timeout and attempts", v1alpha1.SSHSpec{ConnectTimeout: v1alpha1.Duration(10 * time.Second), ConnectionAttempts: 2},
			"pdu.example.org", 182 * time.Second},
		{"through a jump host with a timeout of its own", v1alpha1.SSHSpec{ConnectTimeout: v1alpha1.Duration(10 * time.Second), ConnectionAttempts: 2},
			"behind.example.org", 182*time.Second + 21*time.Second},
		{"through a proxy command", v1alpha1.SSHSpec{ConnectTimeout: v1alpha1.Duration(10 * time.Second), ConnectionAttempts: 2},
			"proxied.example.org", 42 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := transport.New(transport.Options{SSH: tc.ssh, Roles: roles})
			if got := transport.Reach(c, transport.Target{Name: tc.host, Host: tc.host}); got != tc.want {
				t.Errorf("reaching %s may take %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestRunClassifiesTheExitStatus checks the three ways a command can end: the
// host answered with a failure, ssh could not reach it, or it succeeded.
// ExitResult has to agree, since the tests of every command rely on it to
// fail the way Run does.
func TestRunClassifiesTheExitStatus(t *testing.T) {
	tests := []struct {
		name   string
		script string
		code   int
		exit   int
		detail string
	}{
		{"the host answered", "echo 'ibwarn: cannot open UMAD port' >&2; exit 1", 1, exitcode.TargetFailed, "command exited 1"},
		{"ssh failed", "echo 'ssh: connect to host exe0001 port 22: Connection refused' >&2; exit 255", 255, exitcode.Transport, "Connection refused"},
		{"it worked", "echo ok", 0, exitcode.OK, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := fakeClient(t, tc.script).Run(context.Background(), target, transport.Request{Argv: []string{"true"}})
			if err != nil {
				t.Fatal(err)
			}
			if result.ExitCode != tc.code {
				t.Errorf("exit status = %d, want %d", result.ExitCode, tc.code)
			}
			if got := exitcode.From(result.Err); got != tc.exit {
				t.Errorf("exit code = %d, want %d (error %v)", got, tc.exit, result.Err)
			}
			if tc.detail != "" && (result.Err == nil || !strings.Contains(result.Err.Error(), tc.detail)) {
				t.Errorf("error = %v, want it to mention %q", result.Err, tc.detail)
			}

			fake := transport.ExitResult(target, result.ExitCode, result.Stdout, result.Stderr)
			if got, want := exitcode.From(fake.Err), exitcode.From(result.Err); got != want {
				t.Errorf("ExitResult gives exit code %d, Run %d", got, want)
			}
			if (fake.Err == nil) != (result.Err == nil) {
				t.Errorf("ExitResult error = %v, Run error = %v", fake.Err, result.Err)
			}
		})
	}
}

// TestRunReturnsWhenADescendantHoldsTheOutput checks that an interrupt ends
// Run even when something ssh started, such as a ProxyCommand, keeps the
// output pipe open. Wait used to block until that process exited.
func TestRunReturnsWhenADescendantHoldsTheOutput(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, "sleep 60 & exec sleep 60")
	start := time.Now()
	result, err := c.Run(cancelSoon(t), target, transport.Request{Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("Run took %v to return after the interrupt", elapsed)
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Errorf("error = %v, want an interrupt", result.Err)
	}
}

// TestRunStopsSshGently checks that an interrupt asks ssh to stop rather than
// killing it outright, so that it can close the session and restore the
// terminal. Only a process that is not killed runs its trap.
func TestRunStopsSshGently(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, `trap 'kill $! 2>/dev/null; echo stopped gently >&2; exit 255' TERM HUP
sleep 60 &
wait`)
	result, err := c.Run(cancelSoon(t), target, transport.Request{Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Stderr, "stopped gently") {
		t.Errorf("ssh was not given the chance to stop; stderr = %q", result.Stderr)
	}
}

// loopback is an ssh that runs the remote command with the local shell, the
// way sshd hands it to the account's shell on the host.
const loopback = `while [ "$1" != "--" ]; do shift; done
shift 2
exec sh -c "$1"`

// TestRemoteStatus255IsNotAConnectionFailure checks that a command exiting
// 255 on a host that answered is not reported as unreachable. ssh reports its
// own failure as 255 too, so "login install -- sh -c 'exit 255'" printed "ssh
// reported a connection failure" and exited 3.
func TestRemoteStatus255IsNotAConnectionFailure(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, loopback)
	for _, req := range []transport.Request{
		{Argv: []string{"sh", "-c", "exit 255"}},
		{Script: "exit 255"},
	} {
		result, err := c.Run(context.Background(), target, req)
		if err != nil {
			t.Fatal(err)
		}
		if got := exitcode.From(result.Err); got != exitcode.TargetFailed {
			t.Errorf("%+v: exit code = %d (%v), want %d", req, got, result.Err, exitcode.TargetFailed)
		}
		if result.ExitCode != 254 {
			t.Errorf("%+v: exit status = %d, want 255 reported as 254", req, result.ExitCode)
		}
	}
}

// TestRemoteStatusIsKept checks that every other status comes back as the
// command gave it, and that the arguments arrive unchanged.
func TestRemoteStatusIsKept(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, loopback)
	for _, code := range []string{"0", "1", "3", "124", "254"} {
		result, err := c.Run(context.Background(), target, transport.Request{Argv: []string{"sh", "-c", "exit " + code}})
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(result.ExitCode); got != code {
			t.Errorf("exit %s came back as %s", code, got)
		}
	}
	result, err := c.Run(context.Background(), target, transport.Request{Argv: []string{"printf", "%s|", "*.log", "it's", "a  b"}})
	if err != nil || result.Failed() {
		t.Fatalf("printf failed: %v %v", err, result.Err)
	}
	if got, want := result.Stdout, "*.log|it's|a  b|"; got != want {
		t.Errorf("the arguments arrived as %q, want %q", got, want)
	}
}

// TestNoShellSendsTheCommandAsItIs checks that a host whose command line is
// not sh, such as a power distribution unit, is not sent the guard it could
// not run.
func TestNoShellSendsTheCommandAsItIs(t *testing.T) {
	t.Parallel()
	c := fakeClient(t, "true")
	for _, tc := range []struct {
		noShell bool
		want    string
	}{
		{true, "show outlets"},
		{false, `sh -c '"$@"; s=$?; [ "$s" -ne 255 ] || s=254; exit "$s"' sh show outlets`},
	} {
		args, err := c.Args(target, transport.Request{Argv: []string{"show", "outlets"}, NoShell: tc.noShell})
		if err != nil {
			t.Fatal(err)
		}
		if got := args[len(args)-1]; got != tc.want {
			t.Errorf("NoShell %v: the command is sent as %q, want %q", tc.noShell, got, tc.want)
		}
	}
}

// TestRunBoundsTheOutput checks that a host cannot fill the memory of the
// process with its output. Run captured all 300000000 bytes a fake ssh
// wrote, with 1054 MiB of heap in use.
func TestRunBoundsTheOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		script string
		limit  int
		want   int
	}{
		{"standard output, the default bound", fmt.Sprintf("head -c %d /dev/zero", transport.DefaultMaxOutput+3<<20), 0, transport.DefaultMaxOutput},
		{"standard output, the request's bound", "head -c 3000000 /dev/zero", 1 << 20, 1 << 20},
		{"standard error", "head -c 3000000 /dev/zero >&2", 1 << 20, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := fakeClient(t, tc.script).Run(context.Background(), target,
				transport.Request{Argv: []string{"true"}, MaxOutput: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(result.Stdout) + len(result.Stderr); got != tc.want {
				t.Errorf("kept %d bytes, want %d", got, tc.want)
			}
			if !result.Truncated || !result.Failed() {
				t.Errorf("truncated = %v, failed = %v; output that was cut off is not the host's answer", result.Truncated, result.Failed())
			}
			if result.Err == nil || !strings.Contains(result.Err.Error(), "cut off") {
				t.Errorf("error = %v, want it to say the output was cut off", result.Err)
			}
		})
	}

	result, err := fakeClient(t, "head -c 1000 /dev/zero").Run(context.Background(), target,
		transport.Request{Argv: []string{"true"}, MaxOutput: 1000})
	if err != nil || result.Failed() || result.Truncated || len(result.Stdout) != 1000 {
		t.Errorf("output exactly at the bound: failed = %v, truncated = %v, kept %d, error %v",
			result.Failed(), result.Truncated, len(result.Stdout), err)
	}
}

// TestNoTerminalNeverPrompts checks that without a terminal ssh and scp run
// in batch mode and in a session of their own. When a host fell back to
// password authentication, a read_command call under the MCP server put
// ssh's password prompt on the terminal the MCP client runs in.
func TestNoTerminalNeverPrompts(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc to read the session from")
	}
	// The fake ssh prints whether it leads its own session, and its options.
	script := `read -r pid _ _ _ _ sid _ < /proc/$$/stat
[ "$sid" = "$pid" ] && echo own || echo shared
echo "$@"`
	for _, tc := range []struct {
		noTerminal bool
		session    string
		batch      bool
	}{{true, "own", true}, {false, "shared", false}} {
		c := fakeClientWith(t, script, transport.Options{NoTerminal: tc.noTerminal})

		result, err := c.Run(context.Background(), target, transport.Request{Argv: []string{"true"}})
		if err != nil || result.Failed() {
			t.Fatalf("Run failed: %v %v", err, result.Err)
		}
		lines := strings.SplitN(result.Stdout, "\n", 2)
		if lines[0] != tc.session {
			t.Errorf("NoTerminal %v: ssh's session is %q, want %q", tc.noTerminal, lines[0], tc.session)
		}
		options, _, _ := strings.Cut(lines[1], " -- ")
		if got := strings.Contains(options, "-o BatchMode=yes"); got != tc.batch {
			t.Errorf("NoTerminal %v: ssh was run as %q; batch mode %v, want %v", tc.noTerminal, lines[1], got, tc.batch)
		}

		copied, err := c.Copy(context.Background(), target, transport.CopyRequest{
			Sources: []string{"/etc/hosts"}, Destination: "/tmp/hosts", Upload: true,
		})
		if err != nil || copied.Failed() {
			t.Fatalf("Copy failed: %v %v", err, copied.Err)
		}
		args, err := c.CopyArgs(target, transport.CopyRequest{Sources: []string{"a"}, Destination: "b", Upload: true})
		if err != nil {
			t.Fatal(err)
		}
		options, _, _ = strings.Cut(strings.Join(args, " "), " -- ")
		if got := strings.Contains(options, "-o BatchMode=yes"); got != tc.batch {
			t.Errorf("NoTerminal %v: scp is run as %q; batch mode %v, want %v", tc.noTerminal, args, got, tc.batch)
		}
	}
}
