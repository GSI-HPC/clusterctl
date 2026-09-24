// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package ipmi_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
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
