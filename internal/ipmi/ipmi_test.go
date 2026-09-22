// SPDX-License-Identifier: LGPL-3.0-or-later

package ipmi_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/ipmi"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func backend(t *testing.T, spec v1alpha1.IPMISpec, out string) (*ipmi.Backend, *capturingRunner) {
	t.Helper()
	runner := &capturingRunner{stdout: out}
	return &ipmi.Backend{
		Runner:   runner,
		Target:   transport.Target{Name: "mgmt", Host: "mgmt-gw.example.org", Role: "mgmt"},
		Spec:     spec,
		Username: "admin",
		Password: "hunter2",
	}, runner
}

// capturingRunner records the script and the payload instead of running them.
type capturingRunner struct {
	script  string
	payload string
	stdout  string
}

func (r *capturingRunner) Run(_ context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
	r.script = req.Script
	if req.Stdin != nil {
		data, err := io.ReadAll(req.Stdin)
		if err != nil {
			return nil, err
		}
		r.payload = string(data)
	}
	return &transport.Result{Target: target, Stdout: r.stdout}, nil
}

func TestPasswordNeverReachesTheArgumentVector(t *testing.T) {
	t.Parallel()

	// ps on a shared gateway shows every argument to every user, so the
	// secret has to travel on stdin into a private file instead.
	for _, spec := range []v1alpha1.IPMISpec{
		{Backend: ipmi.BackendIpmipower},
		{Backend: ipmi.BackendIpmitool},
	} {
		b, runner := backend(t, spec, "bmc1: on\n")
		if _, err := b.Power(context.Background(), ipmi.ActionStatus, nodeset.MustParse("bmc1")); err != nil {
			t.Fatalf("Power failed: %v", err)
		}
		if strings.Contains(runner.script, "hunter2") {
			t.Errorf("%s: the password is in the command:\n%s", spec.Backend, runner.script)
		}
		if !strings.Contains(runner.payload, "hunter2") {
			t.Errorf("%s: the password did not travel over stdin, payload was %q", spec.Backend, runner.payload)
		}
		if !strings.Contains(runner.script, "umask 077") {
			t.Errorf("%s: the secret file is not made private:\n%s", spec.Backend, runner.script)
		}
		if !strings.Contains(runner.script, "trap 'rm -f") {
			t.Errorf("%s: the secret file is not removed again:\n%s", spec.Backend, runner.script)
		}
	}
}

func TestIpmipowerTakesTheWholeHostList(t *testing.T) {
	t.Parallel()

	// Both tools take a host list, and contacting a thousand processors one
	// connection at a time is what made the shell version unusable.
	b, runner := backend(t, v1alpha1.IPMISpec{Driver: "LAN_2_0"}, "")
	_, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc[1-4]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	// The host list is quoted, so the shell on the gateway cannot expand
	// the brackets against files that happen to exist there.
	if !strings.Contains(runner.script, "--hostname 'bmc[1-4]'") {
		t.Errorf("the host list was not passed as one quoted argument:\n%s", runner.script)
	}
	if !strings.Contains(runner.script, "--off") {
		t.Errorf("the action is missing:\n%s", runner.script)
	}
	if !strings.Contains(runner.script, "--driver-type LAN_2_0") {
		t.Errorf("the driver is missing:\n%s", runner.script)
	}
}

func TestIpmitoolLoopsOnTheGateway(t *testing.T) {
	t.Parallel()

	b, runner := backend(t, v1alpha1.IPMISpec{Backend: ipmi.BackendIpmitool}, "")
	_, err := b.Power(context.Background(), ipmi.ActionReset, nodeset.MustParse("bmc[1-2]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if !strings.Contains(runner.script, "for h in bmc1 bmc2") {
		t.Errorf("the loop does not run on the gateway:\n%s", runner.script)
	}
	if !strings.Contains(runner.script, "chassis power reset") {
		t.Errorf("the action is missing:\n%s", runner.script)
	}
}

func TestMultiDimensionalSetsAreExpandedForTheHostList(t *testing.T) {
	t.Parallel()

	// FreeIPMI parses one bracketed range per name and cannot read several
	// numeric dimensions, so such a set has to be expanded for it.
	b, runner := backend(t, v1alpha1.IPMISpec{}, "")
	_, err := b.Power(context.Background(), ipmi.ActionStatus, nodeset.MustParse("bmc[1-2]-ib[0-1]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if strings.Contains(runner.script, "bmc[1-2]-ib[0-1]") {
		t.Errorf("a multi dimensional set was sent to FreeIPMI:\n%s", runner.script)
	}
	if !strings.Contains(runner.script, "bmc1-ib0,bmc1-ib1") && !strings.Contains(runner.script, "'bmc1-ib0,bmc1-ib1") {
		t.Errorf("the set was not expanded:\n%s", runner.script)
	}
}

func TestParsesStatusAndFailures(t *testing.T) {
	t.Parallel()

	out := strings.Join([]string{
		"bmc1: on",
		"bmc2: off",
		"bmc3: connection timeout",
		"",
	}, "\n")
	b, _ := backend(t, v1alpha1.IPMISpec{}, out)
	statuses, err := b.Power(context.Background(), ipmi.ActionStatus, nodeset.MustParse("bmc[1-3]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if got, want := len(statuses), 3; got != want {
		t.Fatalf("got %d statuses, want %d", got, want)
	}
	if got, want := statuses[0].State, "on"; got != want {
		t.Errorf("bmc1 = %q, want %q", got, want)
	}
	if statuses[2].Err == "" {
		t.Errorf("a timeout should be reported as an error, got %+v", statuses[2])
	}
}

func TestUnknownActionAndEmptySetAreReported(t *testing.T) {
	t.Parallel()

	b, _ := backend(t, v1alpha1.IPMISpec{}, "")
	if _, err := b.Power(context.Background(), "explode", nodeset.MustParse("bmc1")); err == nil {
		t.Error("an unknown action should be reported")
	}
	if _, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.New()); err == nil {
		t.Error("an empty set should be reported")
	}

	b.Password = ""
	if _, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc1")); err == nil {
		t.Error("a missing password should be reported before anything is sent")
	}
}
