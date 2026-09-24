// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// fakeHost is an SSH server that presents one Ed25519 host key.
type fakeHost struct {
	address string
	key     string
}

func startFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _, _, _ = ssh.NewServerConn(conn, config)
			}()
		}
	}()
	return &fakeHost{
		address: ln.Addr().String(),
		key:     strings.Fields(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))[1],
	}
}

// dialFake sends every scan to the fake host, whatever it was addressed to.
func dialFake(t *testing.T, host *fakeHost, before func()) {
	t.Helper()
	scanDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		if before != nil {
			before()
		}
		return (&net.Dialer{}).DialContext(ctx, network, host.address)
	}
	t.Cleanup(func() { scanDial = nil })
}

// TestHostkeyVerifyReportsARevokedKey: the marker used to be read as a host
// name, so verify reported "matches" for a key the same file revokes.
func TestHostkeyVerifyReportsARevokedKey(t *testing.T) {
	host := startFakeHost(t)
	dialFake(t, host, nil)

	known := filepath.Join(t.TempDir(), "known_hosts")
	file := "exe0001.hpc.example.org ssh-ed25519 " + host.key + "\n" +
		"@revoked exe0001.hpc.example.org ssh-ed25519 " + host.key + "\n"
	if err := os.WriteFile(known, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := run(t, harnessOptions{}, "--set", "ssh.knownHostsFile="+known, "hostkey", "verify", "-n", "exe1")
	if err == nil {
		t.Fatalf("a revoked key should fail verify:\n%s", h.out)
	}
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(h.out.String(), "REVOKED") {
		t.Errorf("the revocation is not reported:\n%s", h.out)
	}
}

// TestHostkeyRefreshKeepsNotesAndRevocations: a refresh used to drop every
// comment below the first entry and would write a revoked key back.
func TestHostkeyRefreshKeepsNotesAndRevocations(t *testing.T) {
	host := startFakeHost(t)
	dialFake(t, host, nil)

	known := filepath.Join(t.TempDir(), "known_hosts")
	file := "# Host keys of the example site.\n" +
		"exe0002.hpc.example.org ssh-ed25519 AAAAold2\n" +
		"# exe0001 was reinstalled, ticket 4711.\n" +
		"exe0001.hpc.example.org ssh-ed25519 AAAAold1\n" +
		"@revoked exe0002.hpc.example.org ssh-ed25519 " + host.key + "\n"
	if err := os.WriteFile(known, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := run(t, harnessOptions{}, "--set", "ssh.knownHostsFile="+known,
		"hostkey", "refresh", "-y", "-n", "exe[1-2]")
	if err == nil {
		t.Fatalf("a host offering a revoked key should fail the refresh:\n%s", h.out)
	}
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}

	data, err := os.ReadFile(known)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"# Host keys of the example site.\n",
		"# exe0001 was reinstalled, ticket 4711.\nexe0001.hpc.example.org ssh-ed25519 " + host.key + "\n",
		"exe0002.hpc.example.org ssh-ed25519 AAAAold2\n",
		"@revoked exe0002.hpc.example.org ssh-ed25519 " + host.key + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the file lacks %q:\n%s", want, got)
		}
	}
}
