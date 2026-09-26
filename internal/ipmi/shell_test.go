// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package ipmi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/shellquote"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// shellRunner runs the script in a local shell, the way the gateway would.
type shellRunner struct{}

func (shellRunner) Run(ctx context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", req.Script)
	cmd.Stdin = req.Stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	result := &transport.Result{Target: target}
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		result.ExitCode = exit.ExitCode()
	case err != nil:
		return nil, err
	}
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	return result, nil
}

// The ipmitool loop is a shell script, so it is run in a shell: each
// processor's answer and exit code have to come back on its own line, and a
// missing binary has to be a failure rather than a state.
func TestIpmitoolLoopRunsInAShell(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("timeout(1) is not installed")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "ipmitool")
	// The fake answers for bmc1 and fails for any other host, and checks
	// that the password file holds the password.
	script := `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    -H) host=$2; shift ;;
    -f) file=$2; shift ;;
  esac
  shift
done
[ "$(cat "$file")" = 'pa ss#"w\rd' ] || { echo "wrong password" >&2; exit 1; }
if [ "$host" = bmc1 ]; then echo "Chassis Power Control: Down/Off"; exit 0; fi
echo "Set Chassis Power Control to Down/Off failed: Command not supported in present state" >&2
exit 1
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	b := &ipmi.Backend{
		Runner:   shellRunner{},
		Spec:     v1alpha1.IPMISpec{Backend: ipmi.BackendIpmitool, IpmitoolPath: fake},
		Username: "admin",
		Password: `pa ss#"w\rd`,
	}
	statuses, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc[1-2]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if got := statusOf(t, statuses, "bmc1"); got.Err != "" || got.State != "ok" {
		t.Errorf("bmc1 = %+v, want ok", got)
	}
	if got := statusOf(t, statuses, "bmc2"); got.Err != "exit 1: Set Chassis Power Control to Down/Off failed: Command not supported in present state" {
		t.Errorf("bmc2 = %+v, want the failure with its exit code", got)
	}

	b.Spec.IpmitoolPath = filepath.Join(dir, "missing")
	statuses, err = b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc1"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if got := statusOf(t, statuses, "bmc1"); got.Err == "" {
		t.Errorf("a missing ipmitool was taken as an answer: %+v", got)
	}
}

// needsShellTools skips a test when the tools the ipmitool run needs on the
// gateway are not installed here.
func needsShellTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"timeout", "xargs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

// fakeIpmitool writes an ipmitool into dir that runs body with the host it
// was asked about in $host.
func fakeIpmitool(t *testing.T, dir, body string) string {
	t.Helper()
	fake := filepath.Join(dir, "ipmitool")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  case \"$1\" in\n    -H) host=$2; shift ;;\n  esac\n  shift\ndone\n" + body
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return fake
}

// ipmitool was run for one processor after the other, so 40 that did not
// answer took more than 16 minutes. xargs runs it for bmc.ipmi.maxConcurrent
// processors at once, and never for more: each run is held until one more
// than the bound are under way, which never happens while the bound is
// kept, or a patience has passed, so exactly the bound run at once.
func TestIpmitoolRunsForSeveralProcessorsAtOnce(t *testing.T) {
	needsShellTools(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "running"), 0o700); err != nil {
		t.Fatal(err)
	}
	const limit, processors = 3, 6
	fake := fakeIpmitool(t, dir, fmt.Sprintf(`d=%s
touch "$d/running/$host"
echo "$host" >> "$d/started"
i=0
while [ $i -lt 10 ]; do
  n=$(ls "$d/running" | wc -l)
  echo $n >> "$d/seen"
  [ "$n" -gt %d ] && break
  sleep 0.05
  i=$((i+1))
done
rm "$d/running/$host"
echo "Chassis Power Control: Down/Off"
`, shellquote.Quote(dir), limit))

	b := &ipmi.Backend{
		Runner:   shellRunner{},
		Spec:     v1alpha1.IPMISpec{Backend: ipmi.BackendIpmitool, IpmitoolPath: fake, MaxConcurrent: limit},
		Username: "admin",
		Password: "hunter2",
	}
	statuses, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse(fmt.Sprintf("bmc[1-%d]", processors)))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	for _, s := range statuses {
		if s.Err != "" || s.State != "ok" {
			t.Errorf("%s = %+v, want ok", s.BMC, s)
		}
	}
	started := readLines(t, filepath.Join(dir, "started"))
	if len(started) != processors {
		t.Errorf("ipmitool was run for %v, want each of the %d processors once", started, processors)
	}
	peak := 0
	for _, line := range readLines(t, filepath.Join(dir, "seen")) {
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatal(err)
		}
		peak = max(peak, n)
	}
	if peak != limit {
		t.Errorf("ipmitool ran for %d processors at once, want %d", peak, limit)
	}
}

// readLines returns the lines of a file.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// A processor is handed to the run on the gateway as an argument, so a
// name the shell would read otherwise is asked about as it is written,
// and answered for under that name.
func TestIpmitoolTakesEachProcessorAsItIsNamed(t *testing.T) {
	needsShellTools(t)
	dir := t.TempDir()
	fake := fakeIpmitool(t, dir, `printf 'Chassis Power is on\n'`)

	bmcs := nodeset.New()
	names := []string{"bmc$(id)", "bmc`id`", "it's", `bmc"q`, "bmc;true", "bmc*", "fe80::1:2"}
	for _, name := range names {
		if err := bmcs.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	b := &ipmi.Backend{
		Runner:   shellRunner{},
		Spec:     v1alpha1.IPMISpec{Backend: ipmi.BackendIpmitool, IpmitoolPath: fake, MaxConcurrent: 4},
		Username: "ad'min $(x)",
		Password: "hunter2",
	}
	statuses, err := b.Power(context.Background(), ipmi.ActionStatus, bmcs)
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	for _, name := range names {
		if s := statusOf(t, statuses, name); s.Err != "" || s.State != "on" {
			t.Errorf("%q = %+v, want it asked about and on", name, s)
		}
	}
}

// A line cut in the middle of a character loses what was left of it, so
// that what is shown is text, and reads as cut.
func TestIpmitoolLinesCutWithinACharacter(t *testing.T) {
	needsShellTools(t)
	dir := t.TempDir()
	// "bmc1: exit 1: bmc1 " is 19 bytes, so the cut at 400 falls within
	// the 191st two-byte character.
	fake := fakeIpmitool(t, dir, fmt.Sprintf("echo \"$host %s\" >&2\nexit 1\n", strings.Repeat("é", 500)))
	b := &ipmi.Backend{
		Runner:   shellRunner{},
		Spec:     v1alpha1.IPMISpec{Backend: ipmi.BackendIpmitool, IpmitoolPath: fake, MaxConcurrent: 1},
		Username: "admin",
		Password: "hunter2",
	}
	statuses, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc1"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	want := "exit 1: bmc1 " + strings.Repeat("é", 190) + "…"
	if s := statusOf(t, statuses, "bmc1"); s.Err != want {
		t.Errorf("bmc1: error = %q, want %q", s.Err, want)
	}
}

// The runs side by side print into one pipe, so each processor's line is
// written whole with one printf, and cut short enough that the write is
// never mixed with another's, however much ipmitool said.
func TestIpmitoolLinesDoNotMix(t *testing.T) {
	needsShellTools(t)
	dir := t.TempDir()
	long := strings.Repeat("x", 1000)
	fake := fakeIpmitool(t, dir, fmt.Sprintf("echo \"$host %s\" >&2\nexit 1\n", long))

	const processors = 24
	b := &ipmi.Backend{
		Runner:   shellRunner{},
		Spec:     v1alpha1.IPMISpec{Backend: ipmi.BackendIpmitool, IpmitoolPath: fake, MaxConcurrent: processors},
		Username: "admin",
		Password: "hunter2",
	}
	statuses, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse(fmt.Sprintf("bmc[1-%d]", processors)))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if len(statuses) != processors {
		t.Fatalf("got %d statuses, want %d", len(statuses), processors)
	}
	for _, s := range statuses {
		// The line is the name, ": ", and what it says, cut at 400 bytes,
		// and marked as cut.
		want := ("exit 1: " + s.BMC + " " + long)[:400-len(s.BMC)-len(": ")] + "…"
		if s.Err != want {
			t.Errorf("%s: error = %q, want %q", s.BMC, s.Err, want)
		}
	}
}
