// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// writeConfig writes one configuration document into a directory of its own
// and returns the directory, for harnessOptions.config.
func writeConfig(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeTool writes an executable shell script into dir under name.
func fakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// shellRunner runs each script sent to the hosts of one role, "" for the
// nodes, in a real sh, in a scratch directory, with the fake tools of bin
// first on PATH. That is as close to the host as a test gets: what is
// asserted is what the script does, not how it is spelt. Every other host
// answers with nothing.
func shellRunner(t *testing.T, bin, role string) (*transport.Recorder, string) {
	t.Helper()
	work := t.TempDir()
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if tg.Role != role {
			return &transport.Result{Target: tg}, nil
		}
		script := req.Script
		if script == "" {
			script = strings.Join(req.Argv, " ")
		}
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = work
		cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin:/bin"}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return nil, err
			}
			code = exitErr.ExitCode()
		}
		return &transport.Result{Target: tg, Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}, nil
	}}
	return rec, work
}

// TestFabricStateKeepsNodeNamesOutOfTheScript is the report's 1.3: a node
// name from the inventory was written bare into the script that runs as root
// on the fabric host, so $(...) in a name ran there.
func TestFabricStateKeepsNodeNamesOutOfTheScript(t *testing.T) {
	const evil = "$(touch${IFS}PWNED)"
	inventory := writeConfig(t, "inventory.yaml", `
apiVersion: clusterctl/v1alpha1
kind: NodeInventory
metadata:
  name: example
spec:
  nodes:
    - nodes: exe0001
      attributes: {class: gpu}
      macs: ["00:11:22:33:44:55"]
    - nodes: '`+evil+`'
      attributes: {class: gpu}
      macs: ["00:11:22:33:44:77"]
`)
	bin := t.TempDir()
	rec, work := shellRunner(t, bin, "fabric")

	_, err := run(t, harnessOptions{recorder: rec, config: []string{inventory}}, "fabric", "state", "-n", "@gpu")
	calls := rec.Calls()
	if len(calls) == 0 {
		// Refusing such a name before anything is sent is as good.
		if err == nil {
			t.Fatal("nothing was sent, and nothing was refused")
		}
		return
	}
	for _, c := range calls {
		if strings.Contains(c.Request.Script, "touch") {
			t.Errorf("the node name reached the script:\n%s", c.Request.Script)
		}
	}
	if _, statErr := os.Stat(filepath.Join(work, "PWNED")); statErr == nil {
		t.Error("the node name ran as a command on the fabric host")
	}
}

// ibportstateOutput is what ibportstate prints for a port, abridged.
func ibportstateOutput(state, phys string) string {
	return `CA PortInfo:
# Port info: Lid 5 port 1
LinkState:.......................` + state + `
PhysLinkState:...................` + phys + `
Lid:.............................5
LinkWidthSupported:..............1X or 4X
LinkWidthEnabled:................1X or 4X
LinkWidthActive:.................4X
LinkSpeedSupported:..............2.5 Gbps or 5.0 Gbps or 10.0 Gbps
LinkSpeedEnabled:................2.5 Gbps or 5.0 Gbps or 10.0 Gbps
LinkSpeedActive:.................10.0 Gbps
`
}

// TestFabricStateIsUpOnlyForAnActiveLink is the report's 12.7: the field
// names LinkWidthActive and LinkSpeedActive contain "Active", so every port
// that answered was reported up, whatever its link state.
func TestFabricStateIsUpOnlyForAnActiveLink(t *testing.T) {
	tests := []struct {
		state, phys string
		up          bool
	}{
		{"Active", "LinkUp", true},
		{"Initialize", "LinkUp", false},
		{"Armed", "LinkUp", false},
		{"Down", "Polling", false},
		// Some versions print the number of the state before its name.
		{"4: Active", "5: LinkUp", true},
	}
	for _, tc := range tests {
		t.Run(tc.state+"/"+tc.phys, func(t *testing.T) {
			bin := t.TempDir()
			// ibportstate takes the destination, then the port number, then
			// the operation; anything else is a usage error.
			fakeTool(t, bin, "ibportstate", `
[ "$1" = -G ] && [ "$2" = 0x0011220300334455 ] && [ "$3" = 1 ] && [ "$4" = query ] || { echo "usage" >&2; exit 1; }
cat <<'EOF'
`+ibportstateOutput(tc.state, tc.phys)+`EOF
`)
			rec, _ := shellRunner(t, bin, "fabric")
			h, err := run(t, harnessOptions{recorder: rec}, "fabric", "state", "-n", "exe0001", "-o", "json")
			if tc.up && err != nil {
				t.Fatalf("an active port failed the command: %v\n%s", err, h.out)
			}
			if !tc.up {
				if err == nil {
					t.Fatalf("a port in %s was reported up:\n%s", tc.state, h.out)
				}
				wantCode(t, err, exitcode.TargetFailed)
			}
			var got []struct {
				Node, State, LinkState, PhysicalState string
			}
			if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, h.out)
			}
			if len(got) != 1 || got[0].Node != "exe0001" {
				t.Fatalf("output = %+v, want one entry for exe0001", got)
			}
			if (got[0].State == "up") != tc.up {
				t.Errorf("state = %q for %s/%s", got[0].State, tc.state, tc.phys)
			}
			if !strings.Contains(tc.phys, got[0].PhysicalState) || got[0].PhysicalState == "" {
				t.Errorf("physical state = %q, want it shown as %q", got[0].PhysicalState, tc.phys)
			}
		})
	}
}

func TestFabricStateReportsAPortWithoutAnswer(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "ibportstate", "exit 1\n")
	rec, _ := shellRunner(t, bin, "fabric")
	h, err := run(t, harnessOptions{recorder: rec}, "fabric", "state", "-n", "exe0001")
	if err == nil {
		t.Fatalf("a port without an answer was accepted:\n%s", h.out)
	}
	if !strings.Contains(h.out.String(), "no answer") {
		t.Errorf("the port is not reported as unanswered:\n%s", h.out)
	}
}

// fabricAnswers answers the fabric's script with stdout and the exit status
// given, the way the transport would, and the DHCP server with the example
// configuration, where exe0002's hardware address comes from.
func fabricAnswers(stdout string, code int, stderr string) *transport.Recorder {
	dhcp := dhcpServer("10.0.2")
	return &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if tg.Role == "fabric" {
			return transport.ExitResult(tg, code, stdout, stderr), nil
		}
		return dhcp(tg, req)
	}}
}

// fabric state failed without a row when its script did not finish, so the
// ports the fabric had answered for were lost with the one that held it up.
// A script that stopped early is read for what it printed, and the command
// fails with the reason it stopped; one that printed nothing still fails
// with nothing to show.
func TestFabricStateKeepsThePortsThatAnswered(t *testing.T) {
	const answered = "0|Active|LinkUp|4X|10.0 Gbps\n1|"
	for _, tc := range []struct {
		name, stdout, stderr string
		status, code         int
		detail               string
	}{
		{"the script ran out of time", answered, "", 124, exitcode.TargetFailed, "command exited 124"},
		{"the connection dropped", answered, "Connection to fabric closed by remote host.\n", 255, exitcode.Transport, "closed by remote host"},
		{"the fabric host could not be reached", "", "ssh: connect to host fabric port 22: No route to host\n", 255, exitcode.Transport, "No route to host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := fabricAnswers(tc.stdout, tc.status, tc.stderr)
			h, err := run(t, harnessOptions{recorder: rec}, "fabric", "state", "-n", "exe0001,exe0002", "-o", "json")
			wantCode(t, err, tc.code)
			if err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Errorf("error = %v, want it to say %q", err, tc.detail)
			}
			if tc.stdout == "" {
				if h.out.Len() != 0 {
					t.Errorf("a fabric host that printed nothing gave rows:\n%s", h.out)
				}
				return
			}
			var got []portState
			if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, h.out)
			}
			if len(got) != 2 || got[0].Node != "exe0001" || got[0].State != portUp || got[1].State != portNoAnswer {
				t.Errorf("ports = %+v, want exe0001 up and exe0002 without an answer", got)
			}
		})
	}
}

// An interrupt while the fabric answers prints no table: the ports not yet
// asked would read as if they had not answered. The command exits 130.
func TestFabricStatePrintsNothingOnceInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		cancel()
		return &transport.Result{Target: tg, Stdout: "0|Active|LinkUp|4X|10.0 Gbps\n", ExitCode: 255,
			Err: exitcode.Wrap(exitcode.Interrupted, context.Canceled)}, nil
	}}
	h, err := run(t, harnessOptions{ctx: ctx, recorder: rec}, "fabric", "state", "-n", "exe0001,exe0002", "-o", "json")
	wantCode(t, err, exitcode.Interrupted)
	if h.out.Len() != 0 {
		t.Errorf("an interrupted fabric state printed:\n%s", h.out)
	}
}

// fabric state ran its script through the runner a dry run records into, so
// a dry run reported every port without an answer and failed. It only reads,
// and a dry run asks the fabric as the real run does.
func TestFabricStateAsksTheFabricInADryRun(t *testing.T) {
	rec := fabricAnswers("0|Active|LinkUp|4X|10.0 Gbps\n", 0, "")
	h, err := run(t, harnessOptions{recorder: rec}, "fabric", "state", "-n", "exe0001", "--dry-run")
	if err != nil {
		t.Fatalf("fabric state --dry-run failed: %v\n%s", err, h.out)
	}
	if !strings.Contains(h.out.String(), "Active") {
		t.Errorf("the dry run did not show what the fabric said:\n%s", h.out)
	}
}

// TestFabricCountersUplinkReadsTheCabledSwitchPort is the report's 12.7:
// --uplink named no port at all and dumped every switch port of the fabric.
func TestFabricCountersUplinkReadsTheCabledSwitchPort(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		out := ""
		switch {
		case len(req.Argv) > 0 && req.Argv[0] == "ibaddr":
			out = "GID fe80::11:2203:33:4455 LID start 0x5 end 0x5\n"
		case len(req.Argv) > 0 && req.Argv[0] == "iblinkinfo":
			out = `0x0002c90200404ad8 "SwitchX -  Mellanox Technologies"      4    3[  ] ==( 4X 25.78 Gbps Active/  LinkUp)==>  0x0002c903000e0b72     15    1[  ] "exe0002 HCA-1" ( )
0x0002c90200404ad8 "SwitchX -  Mellanox Technologies"      4   17[  ] ==( 4X 25.78 Gbps Active/  LinkUp)==>  0x0011220300334455      5    1[  ] "exe0001 HCA-1" ( )
0x0002c90200404ad8 "SwitchX -  Mellanox Technologies"      4   18[  ] ==(                Down/ Polling)==>             [  ] "" ( )
`
		case len(req.Argv) > 0 && req.Argv[0] == "perfquery":
			out = "# Port counters: Lid 4 port 17\nSymbolErrorCounter:..............0\n"
		}
		return &transport.Result{Target: tg, Stdout: out}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "fabric", "counters", "exe0001", "--uplink")
	if err != nil {
		t.Fatalf("fabric counters --uplink failed: %v", err)
	}
	calls := rec.Calls()
	last := calls[len(calls)-1].Request.Argv
	if got, want := strings.Join(last, " "), "perfquery 4 17"; got != want {
		t.Errorf("the counters were read with %q, want %q", got, want)
	}
	if !strings.Contains(h.errOut.String(), "port 17") {
		t.Errorf("the switch port is not named:\n%s", h.errOut)
	}
}

func TestFabricCountersUplinkRefusesToGuess(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		out := ""
		if req.Argv[0] == "ibaddr" {
			out = "GID fe80::11:2203:33:4455 LID start 0x5 end 0x5\n"
		}
		return &transport.Result{Target: tg, Stdout: out}, nil
	}}
	_, err := run(t, harnessOptions{recorder: rec}, "fabric", "counters", "exe0001", "--uplink")
	if err == nil {
		t.Fatal("a port with no switch port linked to it was read anyway")
	}
	for _, c := range rec.Calls() {
		if c.Request.Argv[0] == "perfquery" || c.Request.Argv[0] == "ibqueryerrors" {
			t.Errorf("counters were read without a switch port: %v", c.Request.Argv)
		}
	}
}

// TestFabricGUIDReportsNodesItCannotIdentify is the report's 12.7: a node
// without an identifier was left out of -o json and the command exited 0.
func TestFabricGUIDReportsNodesItCannotIdentify(t *testing.T) {
	h, err := run(t, harnessOptions{}, "fabric", "guid", "-n", "exe[1-3]", "-o", "json")
	if err == nil {
		t.Fatalf("nodes without an identifier were not reported:\n%s", h.out)
	}
	wantCode(t, err, exitcode.TargetFailed)
	for _, node := range []string{"exe0002", "exe0003"} {
		if !strings.Contains(h.errOut.String(), node) {
			t.Errorf("%s is not named on standard error:\n%s", node, h.errOut)
		}
	}
	var object map[string][]string
	if err := json.Unmarshal(h.out.Bytes(), &object); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.out)
	}
	if len(object["exe0001"]) != 1 {
		t.Errorf("exe0001 is missing from the output: %v", object)
	}
}

func TestFabricGUIDFailsWhenDHCPCannotBeRead(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return nil, errors.New("connection refused")
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "fabric", "guid", "-n", "exe0001")
	if err == nil {
		t.Fatalf("an unreachable DHCP host was not reported:\n%s", h.out)
	}
	wantCode(t, err, exitcode.Transport)
}

// TestFabricGUIDPrefersDHCP is the report's 12.10: the inventory address won
// whenever there was one, the reverse of the documented order, so a stale
// inventory entry named the wrong port.
func TestFabricGUIDPrefersDHCP(t *testing.T) {
	rec := &transport.Recorder{Responses: []*transport.Result{{
		Stdout: "host exe0001 {\n  hardware ethernet aa:bb:cc:11:22:33;\n  fixed-address 10.0.2.1;\n}\n",
	}}}
	h, err := run(t, harnessOptions{recorder: rec}, "fabric", "guid", "-n", "exe0001")
	if err != nil {
		t.Fatalf("fabric guid failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "0xaabbcc0300112233") {
		t.Errorf("the identifier is not the one DHCP knows:\n%s", h.out)
	}
	if strings.Contains(h.out.String(), "0x0011220300334455") {
		t.Errorf("the inventory address was used although DHCP knows the node:\n%s", h.out)
	}
}

// TestHCAConfigSetRefusesWhatIsNotASetting is the report's 1.6: the key and
// value were written bare into the script, so a value of "1; reboot" ran
// reboot on every selected node.
func TestHCAConfigSetRefusesWhatIsNotASetting(t *testing.T) {
	for _, args := range [][]string{
		{"KEEP_LINK_UP_ON_BOOT_P1", "1; reboot"},
		{"KEEP_LINK_UP_ON_BOOT_P1=1;reboot", "1"},
		{"$(reboot)", "1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, append(append([]string{"hca", "config", "set"}, args...), "-n", "exe0001", "-y")...)
			if err == nil {
				t.Fatalf("%q was accepted", args)
			}
			wantCode(t, err, exitcode.Usage)
			if n := len(h.recorder.Calls()); n != 0 {
				t.Errorf("%d requests were sent", n)
			}
		})
	}
}

func TestHCAConfigSetQuotesTheSetting(t *testing.T) {
	h, err := run(t, harnessOptions{}, "hca", "config", "set", "MODULE_SPLIT_M0[1..3]", "1", "-n", "exe0001", "-y")
	if err != nil {
		t.Fatalf("hca config set failed: %v", err)
	}
	calls := h.recorder.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if !strings.Contains(calls[0].Request.Script, "set 'MODULE_SPLIT_M0[1..3]=1'") {
		t.Errorf("the setting is not quoted:\n%s", calls[0].Request.Script)
	}
}

// TestHCAConfigNeverTakesANodeForTheValue is the report's 12.10: with
// CLUSTERCTL_NODES set, "hca config KEY exe0001" prepared a write of
// KEY=exe0001 to the whole session set.
func TestHCAConfigNeverTakesANodeForTheValue(t *testing.T) {
	t.Setenv("CLUSTERCTL_NODES", "exe[1-4]")
	h, err := run(t, harnessOptions{}, "hca", "config", "KEEP_LINK_UP_ON_BOOT_P1", "exe0001", "-y")
	if err == nil {
		t.Fatal("the old form was accepted")
	}
	wantCode(t, err, exitcode.Usage)
	if n := len(h.recorder.Calls()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}

	h, err = run(t, harnessOptions{}, "hca", "config", "get", "KEEP_LINK_UP_ON_BOOT_P1", "exe0001")
	if err != nil {
		t.Fatalf("hca config get failed: %v", err)
	}
	calls := h.recorder.Calls()
	if len(calls) != 1 || calls[0].Target.Name != "exe0001" {
		t.Fatalf("the read went to %v, want exe0001 only", calls)
	}
	if strings.Contains(calls[0].Command, "set") {
		t.Errorf("a read sent a write: %s", calls[0].Command)
	}
}

// TestHCAConfigSetChangesEveryAdapter is the report's 12.10: only the first
// adapter ibstat listed was changed, and the node was reported ok.
func TestHCAConfigSetChangesEveryAdapter(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "ibstat", `[ "$1" = -l ] && printf 'mlx5_0\nmlx5_1\n'`+"\n")
	fakeTool(t, bin, "mlxconfig", `
echo "$*" >> mlxconfig.log
[ "$3" = mlx5_1 ] && { echo "-E- Failed to set configuration" >&2; exit 1; }
exit 0
`)
	rec, work := shellRunner(t, bin, "")
	h, err := run(t, harnessOptions{recorder: rec}, "hca", "config", "set", "KEEP_LINK_UP_ON_BOOT_P1", "1", "-n", "exe0001", "-y")
	if err == nil {
		t.Fatalf("a node with an adapter left unchanged was reported ok:\n%s", h.out)
	}
	wantCode(t, err, exitcode.TargetFailed)
	log, _ := os.ReadFile(filepath.Join(work, "mlxconfig.log"))
	for _, dev := range []string{"mlx5_0", "mlx5_1"} {
		if !strings.Contains(string(log), dev) {
			t.Errorf("%s was not changed; mlxconfig ran with:\n%s", dev, log)
		}
	}
	out := h.out.String()
	if !strings.Contains(out, "mlx5_0") || !strings.Contains(out, "mlx5_1") || !strings.Contains(out, "failed") {
		t.Errorf("the adapters are not reported one by one:\n%s", out)
	}
}
