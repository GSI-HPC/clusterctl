// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func testClient(t *testing.T) *transport.Client {
	t.Helper()
	dir := t.TempDir()
	system := filepath.Join(dir, "ssh_config.system")
	if err := os.WriteFile(system, []byte("# system\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return transport.New(transport.Options{
		SSH: v1alpha1.SSHSpec{
			Include:             []string{system, filepath.Join(dir, "missing")},
			ConnectTimeout:      v1alpha1.Duration(10 * time.Second),
			ConnectionAttempts:  2,
			ServerAliveInterval: v1alpha1.Duration(60 * time.Second),
			ServerAliveCountMax: 3,
			SendEnv:             []string{"APPTAINER_CONTAINER"},
			Options:             map[string]string{"Compression": "yes"},
		},
		Roles: map[string]v1alpha1.HostRole{
			"mgmt":  {Host: "mgmt-gw.example.org", ForwardAgent: true, ControlMaster: true},
			"dhcp":  {Host: "dhcp01.example.org", User: "root", ProxyJump: "mgmt", LegacyAlgorithms: true},
			"login": {Host: "login.hpc.example.org", Description: "Slurm clients"},
		},
		StateDir:       dir,
		KnownHostsFile: filepath.Join(dir, "ssh-known-hosts"),
		DefaultUser:    "alice_adm",
	})
}

func generatedConfig(t *testing.T) string {
	t.Helper()
	c := testClient(t)
	path, err := c.ConfigPath()
	if err != nil {
		t.Fatalf("generating the ssh configuration: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("the generated configuration has mode %v, want %v", got, want)
	}
	return string(data)
}

func TestGeneratedConfigCarriesTrustSettings(t *testing.T) {
	t.Parallel()
	cfg := generatedConfig(t)

	// Trust settings belong in the file rather than in -o options, because
	// -o does not reach the hops of a ProxyJump.
	for _, want := range []string{
		"Host *",
		"UserKnownHostsFile ",
		"StrictHostKeyChecking yes",
		"CheckHostIP no",
		"HashKnownHosts no",
		"ConnectTimeout 10",
		"ConnectionAttempts 2",
		"ServerAliveInterval 60",
		"SendEnv APPTAINER_CONTAINER",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the generated configuration is missing %q:\n%s", want, cfg)
		}
	}
}

func TestGeneratedConfigIncludesSystemConfigLast(t *testing.T) {
	t.Parallel()
	cfg := generatedConfig(t)

	include := strings.Index(cfg, "Include ")
	if include < 0 {
		t.Fatalf("the system configuration is not included:\n%s", cfg)
	}
	// ssh keeps the first value it obtains, so what clusterctl sets has to
	// come before the include for it to win, and the include has to be
	// there at all for crypto policies and GSSAPI to keep applying.
	if host := strings.Index(cfg, "Host *"); host > include {
		t.Errorf("the include comes before the settings, so the settings would not win:\n%s", cfg)
	}
	// A file that does not exist is left out rather than making ssh fail.
	if strings.Contains(cfg, "missing") {
		t.Errorf("a missing include file was written:\n%s", cfg)
	}
}

func TestGeneratedConfigUsesRealHostNames(t *testing.T) {
	t.Parallel()
	cfg := generatedConfig(t)

	// The block matches the real host, not an invented alias, so that the
	// administrator's own blocks for that host still apply.
	if !strings.Contains(cfg, "Host mgmt-gw.example.org") {
		t.Errorf("the role block does not match the real host name:\n%s", cfg)
	}
	if strings.Contains(cfg, "\nHost mgmt\n") {
		t.Errorf("an alias block was generated:\n%s", cfg)
	}
	// The jump role names no account, so the context's goes with it.
	if !strings.Contains(cfg, "ProxyJump alice_adm@mgmt-gw.example.org") {
		t.Errorf("the jump host was not resolved to a real host and account:\n%s", cfg)
	}
}

func TestGeneratedConfigSpellsLegacyKeywordTheOldWay(t *testing.T) {
	t.Parallel()
	cfg := generatedConfig(t)

	// OpenSSH 8.0, which RHEL 8 ships, aborts on PubkeyAcceptedAlgorithms
	// even inside a Host block that does not match.
	if !strings.Contains(cfg, "PubkeyAcceptedKeyTypes +ssh-rsa") {
		t.Errorf("the legacy algorithm keyword is missing:\n%s", cfg)
	}
	if strings.Contains(cfg, "PubkeyAcceptedAlgorithms") {
		t.Errorf("the keyword RHEL 8 rejects was written:\n%s", cfg)
	}
}

func TestControlMasterOnlyForMarkedRoles(t *testing.T) {
	t.Parallel()
	cfg := generatedConfig(t)

	blocks := strings.SplitSeq(cfg, "\nHost ")
	for block := range blocks {
		if strings.HasPrefix(block, "login.hpc.example.org") && strings.Contains(block, "ControlMaster") {
			t.Errorf("multiplexing was enabled for a role that did not ask for it:\n%s", block)
		}
	}
	if !strings.Contains(cfg, "ControlMaster auto") {
		t.Errorf("the gateway role has no multiplexing:\n%s", cfg)
	}
}

func TestArgs(t *testing.T) {
	t.Parallel()
	c := testClient(t)

	tests := []struct {
		name string
		req  transport.Request
		want []string
	}{
		{
			name: "an interactive login sends no command",
			req:  transport.Request{},
			want: []string{"alice_adm@login.hpc.example.org"},
		},
		{
			name: "a command is one quoted argument",
			req:  transport.Request{Argv: []string{"echo", "*.log", "it's"}},
			want: []string{"--", `echo '*.log' 'it'\''s'`},
		},
		{
			name: "a script is run through the shell",
			req:  transport.Request{Script: "for i in *; do echo $i; done"},
			want: []string{"--", `bash -c 'for i in *; do echo $i; done'`},
		},
		{
			name: "a timeout is enforced on the target",
			req:  transport.Request{Argv: []string{"sleep", "60"}, Timeout: 30 * time.Second},
			want: []string{"--", "timeout -k 5s 30s sleep 60"},
		},
		{
			name: "a terminal can be forced",
			req:  transport.Request{Argv: []string{"top"}, TTY: transport.TTYForce},
			want: []string{"-tt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, err := c.Args(transport.Target{Name: "login", Host: "login.hpc.example.org", Role: "login"}, tc.req)
			if err != nil {
				t.Fatalf("Args failed: %v", err)
			}
			joined := strings.Join(args, "\x00")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("Args = %q, want it to contain %q", args, want)
				}
			}
			if args[0] != "ssh" || args[1] != "-F" {
				t.Errorf("Args = %q, want it to start with ssh -F", args)
			}
		})
	}
}

func TestArgsUsesTheRoleUser(t *testing.T) {
	t.Parallel()
	c := testClient(t)

	args, err := c.Args(transport.Target{Name: "dhcp", Host: "dhcp01.example.org", Role: "dhcp"}, transport.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "root@dhcp01.example.org") {
		t.Errorf("Args = %q, want the role's own account", args)
	}
}

func TestRemoteCommandRejectsBadRequests(t *testing.T) {
	t.Parallel()

	if _, err := transport.RemoteCommand(transport.Request{
		Argv:   []string{"id"},
		Script: "id",
	}); err == nil {
		t.Error("a request with both an argv and a script should be rejected")
	}
	if _, err := transport.RemoteCommand(transport.Request{Timeout: time.Second}); err == nil {
		t.Error("a timeout without a command should be rejected")
	}
	// One argument is capped by the kernel; a payload that large has to go
	// over stdin instead.
	if _, err := transport.RemoteCommand(transport.Request{
		Argv: []string{"echo", strings.Repeat("x", 200000)},
	}); err == nil {
		t.Error("an oversized command should be rejected")
	} else if !strings.Contains(err.Error(), "stdin") {
		t.Errorf("error = %v, want it to suggest stdin", err)
	}
}

func TestCopyArgs(t *testing.T) {
	t.Parallel()
	c := testClient(t)
	target := transport.Target{Name: "login", Host: "login.hpc.example.org", Role: "login"}

	up, err := c.CopyArgs(target, transport.CopyRequest{
		Sources: []string{"local file"}, Destination: "/tmp/x", Upload: true, Recursive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(up, " ")
	if !strings.Contains(joined, "-r") || !strings.Contains(joined, "alice_adm@login.hpc.example.org:/tmp/x") {
		t.Errorf("upload args = %q", up)
	}

	down, err := c.CopyArgs(target, transport.CopyRequest{
		Sources: []string{"/etc/hosts"}, Destination: "-weird",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A destination starting with a dash would otherwise be read as an
	// option by scp.
	if !strings.Contains(strings.Join(down, " "), "./-weird") {
		t.Errorf("download args = %q, want the destination made unambiguous", down)
	}

	if _, err := c.CopyArgs(target, transport.CopyRequest{Destination: "/tmp"}); err == nil {
		t.Error("a copy without a source should be rejected")
	}
	if _, err := c.CopyArgs(target, transport.CopyRequest{Sources: []string{"a"}}); err == nil {
		t.Error("a copy without a destination should be rejected")
	}
}

func TestRecorder(t *testing.T) {
	t.Parallel()

	rec := &transport.Recorder{Responses: []*transport.Result{{Stdout: "exe1\nexe2\n"}}}
	target := transport.Target{Name: "login", Host: "login.hpc.example.org"}

	res, err := rec.Run(context.Background(), target, transport.Request{
		Argv:  []string{"sinfo", "-h", "-o", "%N"},
		Stdin: strings.NewReader("payload"),
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got, want := res.Lines(), []string{"exe1", "exe2"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Lines = %q, want %q", got, want)
	}
	if res.Failed() {
		t.Error("a zero exit should not count as a failure")
	}
	if got, want := rec.Commands()[0], "sinfo -h -o %N"; got != want {
		t.Errorf("recorded command = %q, want %q", got, want)
	}
	if got := rec.Calls()[0].Describe(); !strings.Contains(got, "login") {
		t.Errorf("Describe = %q, want it to name the target", got)
	}

	// Once the prepared responses run out, calls succeed with no output.
	second, err := rec.Run(context.Background(), target, transport.Request{Argv: []string{"true"}})
	if err != nil || second.Failed() {
		t.Errorf("second call = %+v, %v", second, err)
	}
}

func TestResultHelpers(t *testing.T) {
	t.Parallel()

	r := &transport.Result{Stdout: "a\nb\n\n"}
	if got, want := r.Output(), "a\nb"; got != want {
		t.Errorf("Output = %q, want %q", got, want)
	}
	if got := len(r.Lines()); got != 2 {
		t.Errorf("Lines returned %d lines, want 2", got)
	}
	var missing *transport.Result
	if !missing.Failed() {
		t.Error("a nil result must count as a failure")
	}
}

func TestTargetString(t *testing.T) {
	t.Parallel()

	if got, want := (transport.Target{Name: "mgmt", Host: "gw.example.org"}).String(), "mgmt (gw.example.org)"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	if got, want := (transport.Target{Name: "gw", Host: "gw"}).String(), "gw"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestCheckSaysWhyACommandFailed(t *testing.T) {
	t.Parallel()

	target := transport.Target{Name: "login", Host: "login"}
	coded := exitcode.Wrap(exitcode.Transport, errors.New("login: Connection refused"))
	tests := []struct {
		name   string
		result transport.Result
		code   int
		want   string
	}{
		{"success", transport.Result{}, exitcode.OK, ""},
		{"the transport's own failure", transport.Result{ExitCode: 255, Err: coded}, exitcode.Transport, "login: Connection refused"},
		{"no exit status", transport.Result{ExitCode: -1, Err: errors.New("cut off")}, exitcode.Transport, "cut off"},
		{"a refusal", transport.Result{ExitCode: 2, Stderr: "one\n\n  two\n", Stdout: "out\n"},
			exitcode.TargetFailed, "ls on login exited 2: one; two"},
		{"a refusal on standard output", transport.Result{ExitCode: 2, Stdout: "\nfirst\nsecond\n"},
			exitcode.TargetFailed, "ls on login exited 2: first"},
		{"a silent refusal", transport.Result{ExitCode: 2}, exitcode.TargetFailed, "ls on login exited 2"},
		{"what the host said is escaped", transport.Result{ExitCode: 1, Stderr: "a\x1b[2Kb\n"},
			exitcode.TargetFailed, `ls on login exited 1: a\x1b[2Kb`},
	}
	for _, tc := range tests {
		result := tc.result
		result.Target = target
		err := result.Check("ls")
		if got := exitcode.From(err); got != tc.code {
			t.Errorf("%s: exit code = %d (%v), want %d", tc.name, got, err, tc.code)
		}
		if got := fmt.Sprint(err); tc.want != "" && got != tc.want {
			t.Errorf("%s: error = %q, want %q", tc.name, got, tc.want)
		}
	}
}
