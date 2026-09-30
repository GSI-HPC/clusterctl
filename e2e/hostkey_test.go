// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

package e2e

import (
	"crypto/ed25519"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The host key commands read each key with an SSH handshake of their own
// rather than through ssh, so sind's relay does not carry them: they dial
// the nodes directly, whose names have to resolve on this machine and whose
// addresses, on sind's Docker networks, a Linux host reaches.

// hostkeyRow is one host as the host key commands print it with -o json.
type hostkeyRow struct {
	Host   string `json:"host"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// resolvable fails the test unless the nodes can be dialled directly.
func resolvable(t *testing.T) {
	t.Helper()
	if _, err := net.LookupHost("worker-0." + domain); err != nil {
		t.Fatalf(`the host key commands dial the nodes themselves, so their names have to resolve here; `+
			`add what "make e2e-hosts" prints to /etc/hosts: %v`, err)
	}
}

// restoreKnownHosts puts the copy of sind's host keys back once the test is
// over, whatever the test did to it.
func restoreKnownHosts(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := copyFile(sindKnownHosts, knownHosts); err != nil {
			t.Error(err)
		}
	})
}

// statuses maps each host of a host key command's output to its status.
func statuses(t *testing.T, r result) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, row := range decode[[]hostkeyRow](t, r) {
		out[row.Host] = row.Status
	}
	return out
}

// TestHostkeyVerifyMatchesTheKeysSindCollected checks that the keys the
// handshake reads are the ones sind collected with ssh-keyscan on each node.
func TestHostkeyVerifyMatchesTheKeysSindCollected(t *testing.T) {
	resolvable(t)
	r := clusterctl(t, "hostkey", "verify", "controller,submitter,worker-[0-2]", "-o", "json")
	r.wantCode(t, 0)
	got := statuses(t, r)
	for _, node := range append([]string{"controller", "submitter"}, workers...) {
		if host := node + "." + domain; got[host] != "matches" {
			t.Errorf("%s is %q, want matches: %s", host, got[host], r)
		}
	}
}

// TestHostkeyChangedKeyIsRefusedUntilRefreshed gives a node a key in the
// site's file other than the one it offers, as a reinstall would, and checks
// that verify reports it, that ssh refuses the node, and that refresh
// restores the trust.
func TestHostkeyChangedKeyIsRefusedUntilRefreshed(t *testing.T) {
	resolvable(t)
	restoreKnownHosts(t)
	host := "worker-1." + domain
	replaceKey(t, host)

	r := clusterctl(t, "hostkey", "verify", "worker-[0-2]", "-o", "json")
	r.wantCode(t, 1)
	got := statuses(t, r)
	if got[host] != "CHANGED" || got["worker-0."+domain] != "matches" || got["worker-2."+domain] != "matches" {
		t.Errorf("only %s should have changed: %s", host, r)
	}

	r = clusterctl(t, "exec", "-n", "worker-1", "-o", "json", "--", "true")
	r.wantCode(t, 3)
	if rep := replies(t, r, "worker-1")["worker-1"]; !strings.Contains(rep.Stderr+rep.Error, "Host key verification failed") {
		t.Errorf("ssh did not refuse the changed key: %+v", rep)
	}

	r = clusterctl(t, "hostkey", "refresh", "-y", "worker-1", "-o", "json")
	r.wantCode(t, 0)
	if got := statuses(t, r); got[host] != "written" {
		t.Errorf("refresh did not write the key of %s: %s", host, r)
	}

	clusterctl(t, "hostkey", "verify", "worker-[0-2]").wantCode(t, 0)
	r = clusterctl(t, "exec", "-n", "worker-1", "--", "uname", "-n")
	r.wantCode(t, 0)
}

// TestHostkeyVerifyReportsAnUnreachableHost checks the code of a host that
// did not answer, which is not a changed key.
func TestHostkeyVerifyReportsAnUnreachableHost(t *testing.T) {
	r := clusterctl(t, "hostkey", "verify", "worker-9", "-o", "json")
	r.wantCode(t, 3)
	if got := statuses(t, r)["worker-9."+domain]; got != "unreachable" {
		t.Errorf("worker-9 is %q, want unreachable: %s", got, r)
	}
}

// replaceKey gives host a new key in the site's file, one no node has.
func replaceKey(t *testing.T, host string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	replaced := 0
	for i, line := range lines {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == host {
			lines[i] = host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			replaced++
		}
	}
	if replaced == 0 {
		t.Fatalf("%s holds no key of %s:\n%s", knownHosts, host, data)
	}
	if err := os.WriteFile(knownHosts, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}
