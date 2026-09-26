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

// TestCopyShowsAProgressMeterOnlyForOneTransferAtATime: scp's standard
// output, where it draws its meter when that is a terminal, was the
// process's standard error whatever the fan-out, so the meters of transfers
// running side by side overwrote each other. It is the command's error
// stream when one transfer runs at a time, and the null device otherwise,
// where scp draws nothing.
func TestCopyShowsAProgressMeterOnlyForOneTransferAtATime(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantMeter int
	}{
		{"one node", []string{"-n", "exe1"}, 1},
		{"one node at a time", []string{"--fanout", "1", "-n", "exe[1-2]"}, 2},
		{"nodes side by side", []string{"-n", "exe[1-2]"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binary, dir := fakeScp(t, `
[ /dev/stdout -ef /dev/null ] && touch "$(dirname "$0")/discarded.$$"
echo "hosts 100% 1024 1.0MB/s 00:00"
exit 0`)
			args := append([]string{"--set", "ssh.scpBinary=" + binary, "-y", "copy"}, tc.args...)
			h, err := run(t, harnessOptions{}, append(args, "/etc/hosts", "/etc/hosts")...)
			if err != nil {
				t.Fatalf("copy failed: %v", err)
			}
			if got := strings.Count(h.errOut.String(), "hosts 100%"); got != tc.wantMeter {
				t.Errorf("%d meters on the error stream, want %d:\n%s", got, tc.wantMeter, h.errOut)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			discarded := 0
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "discarded.") {
					discarded++
				}
			}
			if want := len(scpCalls(t, dir)) - tc.wantMeter; discarded != want {
				t.Errorf("scp's output went to the null device %d times, want %d", discarded, want)
			}
		})
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

// Each transfer is a call under its node's target, which says how it ended:
// a transfer scp failed is the node's failure, and one given up on at its
// timeout ran out of time.
func TestCopyReportsEachTransferAsACall(t *testing.T) {
	binary, _ := fakeScp(t, `case "$*" in
*exe0002*) echo "scp: /etc/hosts: Permission denied" >&2; exit 1 ;;
*exe0003*) exec sleep 30 ;;
esac`)
	ctx, tree := watch(t)
	_, err := run(t, harnessOptions{ctx: ctx}, "--set", "ssh.scpBinary="+binary, "-y",
		"copy", "-n", "exe[1-3]", "--timeout", "1s", "/etc/hosts", "/etc/hosts")
	wantCode(t, err, exitcode.Transport)
	// A summary takes the class of the first of its failures that says one,
	// the timeout here, where its exit code is the worst of them, 3. The
	// timeout leaves the transfers that end at once a wide margin on a slow
	// machine.
	want := `command copy: failed (timeout): 2 of 3 hosts failed: exe[0002-0003]
  step copy total=3 limit=24 [fold]: failed (timeout): 2 of 3 failed: exe[0002-0003]
    target exe0001: ok
      call scp node={} host={} timeout=1s exit=0: ok
    target exe0002: failed (target): {} ({}): command exited 1
      call scp node={} host={} timeout=1s exit=1: failed (target): {} ({}): command exited 1
    target exe0003: failed (timeout): {} ({}): the transfer did not finish within 1s: context deadline exceeded
      call scp node={} host={} timeout=1s: failed (timeout): {} ({}): the transfer did not finish within 1s: context deadline exceeded
  wait confirm message=copy files to 3 hosts: ok
`
	if got := tree(); got != want {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want)
	}
}
