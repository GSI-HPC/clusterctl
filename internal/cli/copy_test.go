// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// fakeScp writes a script standing in for scp. Each run records its
// arguments, one per line, under dir/calls, then runs body.
func fakeScp(t *testing.T, body string) (binary, dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "calls"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary = filepath.Join(dir, "scp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + dir + "/calls/$$\n" + body + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, dir
}

// scpCalls returns the argument vector of every recorded run, sorted by the
// last source so the order does not depend on scheduling.
func scpCalls(t *testing.T, dir string) [][]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, "calls", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Split(strings.TrimRight(string(data), "\n"), "\n"))
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i], " ") < strings.Join(out[j], " ") })
	return out
}

// TestCopyDownloadsEachNodeIntoItsOwnDirectory: with several sources, the
// destination used to be DESTINATION<node>-<first source's base name>, a
// file path, so scp failed on every node.
func TestCopyDownloadsEachNodeIntoItsOwnDirectory(t *testing.T) {
	binary, dir := fakeScp(t, "exit 0")
	logs := filepath.Join(t.TempDir(), "logs") + "/"

	_, err := run(t, harnessOptions{}, "--set", "ssh.scpBinary="+binary, "-y",
		"copy", "-n", "exe[1-2]", "--download", "/var/log/messages", "/var/log/slurmd.log", logs)
	if err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	calls := scpCalls(t, dir)
	if len(calls) != 2 {
		t.Fatalf("scp ran %d times, want 2", len(calls))
	}
	for i, node := range []string{"exe0001", "exe0002"} {
		args := calls[i]
		want := logs + node + "/"
		if got := args[len(args)-1]; got != want {
			t.Errorf("destination = %q, want %q", got, want)
		}
		if !strings.Contains(strings.Join(args, " "), ":/var/log/messages") ||
			!strings.Contains(strings.Join(args, " "), ":/var/log/slurmd.log") {
			t.Errorf("not every source is fetched: %q", args)
		}
		if info, err := os.Stat(want); err != nil || !info.IsDir() {
			t.Errorf("the directory for %s was not created: %v", node, err)
		}
	}
}

// TestCopyRunsNodesInParallel: the nodes used to be copied one at a time,
// whatever fanout.max said. Each run here waits until all four have started,
// which only happens when they run at once.
func TestCopyRunsNodesInParallel(t *testing.T) {
	binary, dir := fakeScp(t, `
touch "$(dirname "$0")/started.$$"
i=0
while [ "$(ls "$(dirname "$0")" | grep -c '^started\.')" -lt 4 ]; do
	i=$((i+1))
	[ "$i" -gt 40 ] && exit 1
	sleep 0.05
done
exit 0`)

	start := time.Now()
	_, err := run(t, harnessOptions{}, "--set", "ssh.scpBinary="+binary, "-y",
		"copy", "-n", "exe[1-4]", "/etc/hosts", "/etc/hosts")
	if err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	if got := len(scpCalls(t, dir)); got != 4 {
		t.Errorf("scp ran %d times, want 4", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("four nodes took %s; they were not copied in parallel", elapsed)
	}
}

// TestCopyGivesUpOnAStalledTransfer: a transfer stalled on a live connection
// used to hold up every later node until Ctrl-C. The fake scp leaves a child
// holding its standard error, as scp's own ssh does, which must not keep the
// command waiting once the transfer is given up on.
func TestCopyGivesUpOnAStalledTransfer(t *testing.T) {
	binary, _ := fakeScp(t, "sleep 20 >&2 &\nexec sleep 30")

	start := time.Now()
	h, err := run(t, harnessOptions{}, "--set", "ssh.scpBinary="+binary, "-y", "-o", "json",
		"copy", "-n", "exe1", "--timeout", "300ms", "/etc/hosts", "/etc/hosts")
	if err == nil {
		t.Fatal("a stalled transfer should fail")
	}
	// scp is killed at the timeout, and its child is given up on after the
	// transport's grace period.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the stalled transfer held the command for %s", elapsed)
	}
	// A transfer given up on is a connection that did not finish, not a
	// command that failed on the node.
	wantCode(t, err, exitcode.Transport)
	if !strings.Contains(h.out.String()+err.Error(), "exe0001") {
		t.Errorf("the node is not named:\n%s\n%v", h.out, err)
	}
}

// TestCopyRefusesShellSyntaxInRemotePaths: under the legacy scp protocol, the
// default before OpenSSH 9.0, the remote shell splits and expands a remote
// path, while SFTP mode takes it literally. A path the two would read
// differently is refused rather than guessed at.
func TestCopyRefusesShellSyntaxInRemotePaths(t *testing.T) {
	tests := [][]string{
		{"copy", "-n", "exe1", "/etc/hosts", "/tmp/a b"},
		{"copy", "-n", "exe1", "/etc/hosts", "/tmp/$(reboot)"},
		{"copy", "-n", "exe1", "--download", "/var/log/it's", "./here"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			binary, dir := fakeScp(t, "exit 0")
			_, err := run(t, harnessOptions{}, append([]string{"--set", "ssh.scpBinary=" + binary, "-y"}, args...)...)
			if err == nil {
				t.Fatal("the path should be refused")
			}
			wantCode(t, err, exitcode.Usage)
			if got := len(scpCalls(t, dir)); got != 0 {
				t.Errorf("scp ran %d times, want none", got)
			}
		})
	}
}
