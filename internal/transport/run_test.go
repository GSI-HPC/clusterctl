// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"context"
	"errors"
	"fmt"
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
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-ssh")
	// ssh -V is asked for the version before the configuration is written,
	// and is answered at once, whatever the script does otherwise.
	version := "[ \"$1\" = -V ] && { echo OpenSSH_9.6p1 >&2; exit 0; }\n"
	writeScript(t, binary, "#!/bin/sh\n"+version+script+"\n")
	return transport.New(transport.Options{
		SSH:            v1alpha1.SSHSpec{Binary: binary, ScpBinary: binary},
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "ssh-known-hosts"),
	})
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
		{Script: "exit 255", Shell: "sh"},
		{Argv: []string{"sh", "-c", "exit 255"}, Env: map[string]string{"A": "b"}},
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
