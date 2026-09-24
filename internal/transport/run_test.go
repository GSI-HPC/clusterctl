// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	writeScript(t, binary, "#!/bin/sh\n"+script+"\n")
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
