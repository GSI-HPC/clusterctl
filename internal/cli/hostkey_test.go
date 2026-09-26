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
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
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

// fakeSSH writes a script standing in for ssh, which records its arguments
// and fails the way ssh does when a jump host refuses a forward.
func fakeSSH(t *testing.T) (binary, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	binary = filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"echo 'channel 0: open failed: administratively prohibited' >&2\nexit 255\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, argsFile
}

// TestHostkeyScanGoesThroughTheJumpHost: the dhcp role sits behind mgmt, and
// the scanner used to dial it directly from the workstation.
func TestHostkeyScanGoesThroughTheJumpHost(t *testing.T) {
	binary, argsFile := fakeSSH(t)
	h, err := run(t, harnessOptions{},
		"--set", "ssh.binary="+binary, "hostkey", "scan", "-n", "dhcp01", "--timeout", "5s")
	if err == nil {
		t.Fatal("the fake jump host refuses the forward, so the scan should fail")
	}
	wantCode(t, err, exitcode.Transport)
	data, readErr := os.ReadFile(argsFile)
	if readErr != nil {
		t.Fatalf("ssh was not run to reach the jump host: %v; output:\n%s", readErr, h.out)
	}
	args := strings.Fields(string(data))
	joined := strings.Join(args, " ")
	for _, want := range []string{"-F", "BatchMode=yes", "-W dhcp01.example.org:22 -- mgmt-gw.example.org"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh arguments %q do not contain %q", joined, want)
		}
	}
	if !strings.Contains(h.out.String(), "administratively prohibited") {
		t.Errorf("the jump host's reason is not reported:\n%s", h.out)
	}
}

// TestHostkeyScanRunsHostsInParallel: the hosts used to be scanned one at a
// time, so every silent node added a timeout. Each dial here waits until all
// four have started, which only happens when they run at once.
func TestHostkeyScanRunsHostsInParallel(t *testing.T) {
	host := startFakeHost(t)
	var (
		mu      sync.Mutex
		started int
		all     = make(chan struct{})
	)
	dialFake(t, host, func() {
		mu.Lock()
		started++
		if started == 4 {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(3 * time.Second):
		}
	})

	start := time.Now()
	h, err := run(t, harnessOptions{}, "hostkey", "scan", "-n", "exe[1-4]", "--timeout", "10s")
	if err != nil {
		t.Fatalf("hostkey scan failed: %v\n%s", err, h.out)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("four hosts took %s; they were not scanned in parallel", elapsed)
	}
	if got := strings.Count(h.out.String(), "ssh-ed25519"); got != 4 {
		t.Errorf("%d keys collected, want 4:\n%s", got, h.out)
	}
}

// TestHostkeyScanKeepsToTheFanOut: the scans are bounded like any fan-out.
// Each is held until one more than --fanout are under way, which never
// happens while the bound is kept, so exactly that many run at once.
func TestHostkeyScanKeepsToTheFanOut(t *testing.T) {
	host := startFakeHost(t)
	calls := &fanouttest.InFlight{Hold: 3}
	scanDial = calls.Dial(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, host.address)
	})
	t.Cleanup(func() { scanDial = nil })

	h, err := run(t, harnessOptions{}, "--fanout", "2", "hostkey", "scan", "-n", "exe[1-5]", "--timeout", "10s")
	if err != nil {
		t.Fatalf("hostkey scan failed: %v\n%s", err, h.out)
	}
	if got := calls.Peak(); got != 2 {
		t.Errorf("%d hosts were scanned at once, want 2", got)
	}
	if got := calls.Started(); got != 5 {
		t.Errorf("%d hosts were dialled, want 5", got)
	}
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
	wantCode(t, err, exitcode.TargetFailed)
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
	wantCode(t, err, exitcode.TargetFailed)

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

// bmcAddressInventory gives exe0003 a bmcAddress, as a site does for a
// service processor whose DNS record is missing or stale.
func bmcAddressInventory(t *testing.T) string {
	t.Helper()
	return exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0003\n      bmcAddress: 10.9.0.77\n"
	})
}

// TestHostkeyScanReachesTheInventoryBMCAddress: scan --bmc dialled the name
// the naming rules derive, so the key recorded was not that of the service
// processor the bmc commands reach.
func TestHostkeyScanReachesTheInventoryBMCAddress(t *testing.T) {
	host := startFakeHost(t)
	var (
		mu     sync.Mutex
		dialed []string
	)
	scanDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, address)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, host.address)
	}
	t.Cleanup(func() { scanDial = nil })

	h, err := run(t, harnessOptions{config: []string{bmcAddressInventory(t)}},
		"hostkey", "scan", "--bmc", "-n", "exe[0003-0004]", "--timeout", "5s")
	if err != nil {
		t.Fatalf("hostkey scan --bmc failed: %v\n%s", err, h.out)
	}
	got := strings.Join(dialed, " ")
	if !strings.Contains(got, "10.9.0.77:22") || strings.Contains(got, "exe0003") {
		t.Errorf("dialled %q, want exe0003's service processor reached at 10.9.0.77", got)
	}
	if !strings.Contains(got, "exe0004.mgmt.hpc.example.org:22") {
		t.Errorf("dialled %q, want exe0004's service processor reached by its derived name", got)
	}
	if !strings.Contains(h.out.String(), "10.9.0.77") {
		t.Errorf("the key is not reported under 10.9.0.77:\n%s", h.out)
	}
}

// TestHostkeyRemoveTakesTheInventoryBMCAddress: remove --bmc removed the
// entry of the derived name, so the key recorded under the bmcAddress stayed.
func TestHostkeyRemoveTakesTheInventoryBMCAddress(t *testing.T) {
	known := filepath.Join(t.TempDir(), "known_hosts")
	file := "10.9.0.77 ssh-ed25519 AAAArecorded\n" +
		"exe0003.mgmt.hpc.example.org ssh-ed25519 AAAAderived\n"
	if err := os.WriteFile(known, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := run(t, harnessOptions{config: []string{bmcAddressInventory(t)}},
		"--set", "ssh.knownHostsFile="+known, "hostkey", "remove", "--bmc", "-y", "-n", "exe0003")
	if err != nil {
		t.Fatalf("hostkey remove --bmc failed: %v\n%s", err, h.out)
	}
	data, err := os.ReadFile(known)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "10.9.0.77") || !strings.Contains(got, "AAAAderived") {
		t.Errorf("the file is now:\n%s\nwant only the entry of 10.9.0.77 removed", got)
	}
	if !strings.Contains(h.out.String(), "10.9.0.77") {
		t.Errorf("the removal is not reported under 10.9.0.77:\n%s", h.out)
	}
}

// A scan is a step with a target for each node: one whose host answered,
// one whose host did not, one whose service processor has no name, which
// is refused before anything is dialled, and those an interrupt left out,
// which end canceled so that the count still reaches its total.
func TestHostkeyScanReportsEveryNode(t *testing.T) {
	host := startFakeHost(t)
	noBMCName := exampleWith(t, "site.yaml", func(s string) string {
		s = strings.Replace(s, "        bmc: \"{name}.{domains.mgmtHpc}\"\n", "", 1)
		return strings.Replace(s, "        bmc: \"{bmcPrefix}{name}.{domains.mgmt}\"\n", "", 1)
	})
	for _, tc := range []struct {
		name   string
		config []string
		args   []string
		// cancel interrupts the command at the first host dialled;
		// otherwise every host but exe0002 answers.
		cancel bool
		want   string
	}{
		{"a host that does not answer", nil, []string{"hostkey", "scan", "-n", "exe[1-3]"}, false, `
command hostkey scan: failed (transport): 1 of 3 hosts did not answer
  step scan the host keys total=3 limit=24 [fold]: failed (target): 1 of 3 failed: exe0002
    target exe0002: failed (target): no host key could be collected from {}: dial tcp: connection refused
    target exe[0001,0003]: ok
`},
		{"a processor with no name", []string{noBMCName, bmcAddressInventory(t)},
			[]string{"hostkey", "scan", "--bmc", "-n", "exe[1,3]"}, false, `
command hostkey scan: failed (transport): 1 of 2 hosts did not answer
  step scan the host keys total=2 limit=24 [fold]: failed (usage): 1 of 2 failed: exe0001
    target exe0001: failed (usage): naming rule 1, which matches {}, has no bmc template, so its service processor has no name; add one or set bmcAddress in the inventory
    target exe0003: ok
`},
		{"an interrupt", nil, []string{"--fanout", "1", "hostkey", "scan", "-n", "exe[1-3]"}, true, `
command hostkey scan: canceled (canceled): 3 of 3 hosts did not answer
  step scan the host keys total=3 limit=1 [fold]: canceled (canceled): 3 of 3 failed: exe[0001-0003]
    target exe[0001-0003]: canceled (canceled): context canceled
`},
		// The processor with no name is refused first, whatever its place,
		// so that the interrupt does not end it as interrupted.
		{"an interrupt and a processor with no name", []string{noBMCName, bmcAddressInventory(t)},
			[]string{"--fanout", "1", "hostkey", "scan", "--bmc", "-n", "exe[3,5]"}, true, `
command hostkey scan: canceled (canceled): 2 of 2 hosts did not answer
  step scan the host keys total=2 limit=1 [fold]: canceled (canceled): 2 of 2 failed: exe[0003,0005]
    target exe0003: canceled (canceled): context canceled
    target exe0005: failed (usage): naming rule 1, which matches {}, has no bmc template, so its service processor has no name; add one or set bmcAddress in the inventory
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watched, tree := watch(t)
			ctx, cancel := context.WithCancel(watched)
			defer cancel()
			scanDial = func(ctx context.Context, network, address string) (net.Conn, error) {
				switch {
				case tc.cancel:
					cancel()
					<-ctx.Done()
					return nil, ctx.Err()
				case strings.HasPrefix(address, "exe0002."):
					return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
				}
				return (&net.Dialer{}).DialContext(ctx, network, host.address)
			}
			t.Cleanup(func() { scanDial = nil })

			_, err := run(t, harnessOptions{ctx: ctx, config: tc.config}, append(tc.args, "--timeout", "5s")...)
			if err == nil {
				t.Fatal("the scan succeeded")
			}
			if got := tree(); got != tc.want[1:] {
				t.Errorf("progress:\n%s\nwant:\n%s", got, tc.want[1:])
			}
		})
	}
}

// An interrupt stops the scan: hostkey waited for a free slot without
// watching the context, so it went on to scan every remaining host.
func TestHostkeyScanStartsNothingOnceInterrupted(t *testing.T) {
	host := startFakeHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu     sync.Mutex
		dialed int
	)
	scanDial = func(dctx context.Context, network, _ string) (net.Conn, error) {
		mu.Lock()
		dialed++
		mu.Unlock()
		cancel()
		return (&net.Dialer{}).DialContext(dctx, network, host.address)
	}
	t.Cleanup(func() { scanDial = nil })

	h, err := run(t, harnessOptions{ctx: ctx}, "--fanout", "1", "hostkey", "scan", "-n", "exe[1-4]", "--timeout", "5s")
	if err == nil {
		t.Fatalf("the interrupted scan succeeded:\n%s", h.out)
	}
	if dialed != 1 {
		t.Errorf("%d hosts were dialled, want only the one under way when the interrupt came", dialed)
	}
	for _, name := range []string{"exe0001", "exe0002", "exe0003", "exe0004"} {
		if !strings.Contains(h.out.String(), name) {
			t.Errorf("%s is missing from the report:\n%s", name, h.out)
		}
	}
}
