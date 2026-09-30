// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
	"testing"
)

// reply is one node's answer to exec, as -o json prints it.
type reply struct {
	Target struct {
		Name string `json:"name"`
		Host string `json:"host"`
		User string `json:"user"`
	} `json:"target"`
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error"`
}

// replies decodes the answers of exec -o json by node, and fails the test
// unless exactly the nodes given answered.
func replies(t *testing.T, r result, nodes ...string) map[string]reply {
	t.Helper()
	out := map[string]reply{}
	for _, rep := range decode[[]reply](t, r) {
		out[rep.Target.Name] = rep
	}
	if got := slices.Sorted(maps.Keys(out)); !slices.Equal(got, nodes) {
		t.Fatalf("answers from %v, want %v: %s", got, nodes, r)
	}
	return out
}

// TestExecReachesEachNode follows a node name all the way to the container:
// the naming rules make it a host name, the generated ssh configuration
// includes the one sind exports, and its ProxyCommand reaches the node
// through sind's relay. Each node has to answer with its own name, as the
// account the context names.
func TestExecReachesEachNode(t *testing.T) {
	r := clusterctl(t, "exec", "-n", "worker-[0-2]", "-o", "json", "--", "uname", "-n")
	r.wantCode(t, 0)
	for node, rep := range replies(t, r, workers...) {
		if rep.Stdout != node+"\n" || rep.ExitCode != 0 {
			t.Errorf("%s answered %q with %d, want its own name", node, rep.Stdout, rep.ExitCode)
		}
		if rep.Target.Host != node+"."+domain || rep.Target.User != "root" {
			t.Errorf("%s was reached as %s@%s, want root@%s.%s", node, rep.Target.User, rep.Target.Host, node, domain)
		}
	}
}

// TestExecDedupFoldsTheSameAnswers checks that nodes which answered alike
// are shown once, under the node set that answered.
func TestExecDedupFoldsTheSameAnswers(t *testing.T) {
	r := clusterctl(t, "exec", "-n", "worker-[0-2]", "--dedup", "--", "uname", "-s")
	r.wantCode(t, 0)
	if want := "worker-[0-2] (3): ok\n  Linux\n"; r.stdout != want {
		t.Errorf("got\n%s\nwant\n%s", r.stdout, want)
	}
}

// TestExecArgumentsArriveUnchanged sends through ssh and a real remote shell
// what the shell toolkit lost: a glob it expanded on the workstation,
// whitespace it collapsed, an apostrophe that broke the command. The unit
// tests check the quoting against a local shell; this checks it against the
// login shell of the node, at the end of the whole path.
func TestExecArgumentsArriveUnchanged(t *testing.T) {
	args := []string{
		"*.log", "a  b", "it's", `"quoted"`, "$HOME", "${PATH}", "`uname`", "$(uname)",
		"x;y", "a|b", "a&&b", "back\\slash", "tab\there", "new\nline", "", " ", "~", "-n", "--",
	}
	r := clusterctl(t, append([]string{"exec", "-n", "worker-0", "-o", "json", "--", "printf", `%s\0`}, args...)...)
	r.wantCode(t, 0)
	want := strings.Join(args, "\x00") + "\x00"
	if got := replies(t, r, "worker-0")["worker-0"].Stdout; got != want {
		t.Errorf("the node received\n%q\nwant\n%q", got, want)
	}
}

// TestExecStdinArrivesUnchanged sends a payload that is not text, and larger
// than a pipe holds, to several nodes at once; each has to read every byte.
func TestExecStdinArrivesUnchanged(t *testing.T) {
	payload := make([]byte, 256<<10)
	for i := range payload {
		payload[i] = byte(i*7 + i>>8)
	}
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:]) + "  -\n"

	r := clusterctlWithInput(t, string(payload), "exec", "--stdin", "-n", "worker-[0-2]", "-o", "json", "--", "sha256sum")
	r.wantCode(t, 0)
	for node, rep := range replies(t, r, workers...) {
		if rep.Stdout != want {
			t.Errorf("%s read a payload whose digest is %q, want %q", node, rep.Stdout, want)
		}
	}
}

// TestExecScriptRunsInAShell checks that --script hands a program with
// pipes and expansions to a shell on the node rather than to a command.
func TestExecScriptRunsInAShell(t *testing.T) {
	r := clusterctl(t, "exec", "-n", "worker-1", "-o", "json", "--script", `n=$(uname -n); echo "${n%%-*}" | tr a-z A-Z`)
	r.wantCode(t, 0)
	if got := replies(t, r, "worker-1")["worker-1"].Stdout; got != "WORKER\n" {
		t.Errorf("the script printed %q, want %q", got, "WORKER\n")
	}
}

// TestExecExitCodes checks the exit code rule of ADR 0020 with real nodes:
// what a node's command exits with is its own, the status ssh reserves for
// itself never reads as a node that could not be reached, and a node that
// could not be reached decides the code without stopping the others.
// worker-9 is in the inventory and not in sind, so ssh cannot reach it.
func TestExecExitCodes(t *testing.T) {
	tests := []struct {
		name  string
		nodes string
		args  []string
		code  int
		want  map[string]int // the exit code of each node
	}{
		{
			name:  "one node that fails fails the run",
			nodes: "worker-[0-2]",
			args:  []string{"--script", `[ "$(uname -n)" != worker-1 ] || exit 7`},
			code:  1,
			want:  map[string]int{"worker-0": 0, "worker-1": 7, "worker-2": 0},
		},
		{
			name:  "a command exiting 255 failed, and did reach the node",
			nodes: "worker-0",
			args:  []string{"--", "sh", "-c", "exit 255"},
			code:  1,
			want:  map[string]int{"worker-0": 254},
		},
		{
			name:  "a node that cannot be reached is a transport failure",
			nodes: "worker-[0,9]",
			args:  []string{"--", "true"},
			code:  3,
			want:  map[string]int{"worker-0": 0, "worker-9": 255},
		},
		{
			name:  "a node that cannot be reached wins over one that failed",
			nodes: "worker-[1,9]",
			args:  []string{"--", "false"},
			code:  3,
			want:  map[string]int{"worker-1": 1, "worker-9": 255},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := clusterctl(t, append([]string{"exec", "-n", tt.nodes, "-o", "json"}, tt.args...)...)
			r.wantCode(t, tt.code)
			got := replies(t, r, slices.Sorted(maps.Keys(tt.want))...)
			for node, code := range tt.want {
				if got[node].ExitCode != code {
					t.Errorf("%s exited %d, want %d: %+v", node, got[node].ExitCode, code, got[node])
				}
			}
		})
	}
}

// TestExecTimeoutStopsTheCommandOnTheNode checks that a command which runs
// too long is ended on the node itself, by timeout(1), rather than left
// running there when the local ssh goes.
func TestExecTimeoutStopsTheCommandOnTheNode(t *testing.T) {
	// An argument no other process on the node has, to find it by.
	const marker = "3017"
	r := clusterctl(t, "exec", "-n", "worker-0", "-o", "json", "--timeout", "2s", "--", "sleep", marker)
	r.wantCode(t, 1)
	if got := replies(t, r, "worker-0")["worker-0"].ExitCode; got != 124 {
		t.Errorf("the command exited %d, want 124, the status of timeout(1)", got)
	}
	processes := onNode(t, "worker-0", "sh", "-c",
		`for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' < "$f"; echo; done 2>/dev/null`)
	for line := range strings.Lines(processes) {
		if strings.Contains(line, "sleep "+marker) {
			t.Errorf("the command is still running on the node: %s", line)
		}
	}
}

// TestExecRefusesAProtectedHost checks that the controller, a protected host
// of the site, is refused without --force and reached with it.
func TestExecRefusesAProtectedHost(t *testing.T) {
	r := clusterctl(t, "exec", "-n", "controller", "--", "uname", "-n")
	r.wantCode(t, 2)
	if !strings.Contains(r.stderr, "protected host controller") || r.stdout != "" {
		t.Errorf("the refusal does not name the protected host, or the node answered: %s", r)
	}

	r = clusterctl(t, "exec", "--force", "-n", "controller", "-o", "json", "--", "uname", "-n")
	r.wantCode(t, 0)
	if got := replies(t, r, "controller")["controller"].Stdout; got != "controller\n" {
		t.Errorf("with --force the controller answered %q", got)
	}
}

// TestExecDryRunChangesNothing checks that a dry run leaves the node as it
// was: what would be run is shown, and nothing is.
func TestExecDryRunChangesNothing(t *testing.T) {
	const path = "/tmp/clusterctl-e2e-dry-run"
	onNode(t, "worker-0", "rm", "-f", path)
	r := clusterctl(t, "exec", "--dry-run", "-n", "worker-0", "--", "touch", path)
	r.wantCode(t, 0)
	if !strings.Contains(r.stderr, "touch "+path) {
		t.Errorf("the dry run does not show the command: %s", r)
	}
	if _, err := dockerExec("worker-0", "test", "-e", path); err == nil {
		t.Errorf("the dry run created %s on the node", path)
	}
}
