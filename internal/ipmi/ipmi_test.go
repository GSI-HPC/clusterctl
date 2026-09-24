// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package ipmi_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
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
	timeout time.Duration
	stdout  string
	stderr  string
	exit    int
}

func (r *capturingRunner) Run(_ context.Context, target transport.Target, req transport.Request) (*transport.Result, error) {
	r.script = req.Script
	r.timeout = req.Timeout
	if req.Stdin != nil {
		data, err := io.ReadAll(req.Stdin)
		if err != nil {
			return nil, err
		}
		r.payload = string(data)
	}
	return &transport.Result{Target: target, Stdout: r.stdout, Stderr: r.stderr, ExitCode: r.exit}, nil
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

func TestMultiDimensionalSetsHaveOneRangePerNameInTheHostList(t *testing.T) {
	t.Parallel()

	// FreeIPMI is handed at most one bracketed range per name, the form
	// every version of its host list parser reads, without expanding the
	// whole set.
	b, runner := backend(t, v1alpha1.IPMISpec{}, "")
	_, err := b.Power(context.Background(), ipmi.ActionStatus, nodeset.MustParse("bmc[1-2]-ib[0-1]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if strings.Contains(runner.script, "bmc[1-2]-ib[0-1]") {
		t.Errorf("a name with two ranges was sent to FreeIPMI:\n%s", runner.script)
	}
	if !strings.Contains(runner.script, "'bmc1-ib[0-1],bmc2-ib[0-1]'") {
		t.Errorf("the set was not folded along one dimension:\n%s", runner.script)
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

// statusOf returns the status reported for one service processor.
func statusOf(t *testing.T, statuses []ipmi.Status, bmc string) ipmi.Status {
	t.Helper()
	for _, s := range statuses {
		if s.BMC == bmc {
			return s
		}
	}
	t.Fatalf("no status for %s in %+v", bmc, statuses)
	return ipmi.Status{}
}

// A backend that timeout(1) killed after one line passed as a normal run,
// and the processors it never reached dropped out of the result, so the
// command exited 0 with one row for four nodes.
func TestEveryRequestedProcessorIsReported(t *testing.T) {
	t.Parallel()

	b, runner := backend(t, v1alpha1.IPMISpec{}, "bmc1: ok\n")
	runner.exit = 124
	statuses, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc[1-4]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if got, want := len(statuses), 4; got != want {
		t.Fatalf("got %d statuses, want %d: %+v", got, want, statuses)
	}
	if s := statusOf(t, statuses, "bmc1"); s.Err != "" {
		t.Errorf("bmc1 answered ok, got %+v", s)
	}
	for _, bmc := range []string{"bmc2", "bmc3", "bmc4"} {
		s := statusOf(t, statuses, bmc)
		if s.Err == "" || !strings.Contains(s.Err, "timeout") {
			t.Errorf("%s was never reported and the backend timed out, got %+v", bmc, s)
		}
		if got := exitcode.From(s.Cause); got != exitcode.Transport {
			t.Errorf("%s: exit code of the cause = %d, want %d", bmc, got, exitcode.Transport)
		}
	}

	// Nothing at all with exit 0 is not a success either.
	b, _ = backend(t, v1alpha1.IPMISpec{}, "")
	statuses, err = b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc[1-2]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if len(statuses) != 2 || statuses[0].Err == "" || statuses[1].Err == "" {
		t.Errorf("an empty answer was taken as success: %+v", statuses)
	}
}

// Only a list of failure words was recognised, so anything else a tool said
// became the state of the node and the command exited 0.
func TestOnlyKnownAnswersCountAsSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend string
		action  string
		out     string
		stderr  string
		exit    int
		ok      map[string]string // processor to state, for the ones that succeeded
		failed  []string
	}{
		{
			name: "ipmipower busy and cipher", backend: ipmi.BackendIpmipower, action: ipmi.ActionOff,
			out: "bmc1: BMC busy\nbmc2: cipher suite id unavailable\nbmc3: ok\n",
			ok:  map[string]string{"bmc3": "ok"}, failed: []string{"bmc1", "bmc2"},
		},
		{
			name: "ipmipower status", backend: ipmi.BackendIpmipower, action: ipmi.ActionStatus,
			out: "bmc1: on\nbmc2: off\nbmc3: unknown\n",
			ok:  map[string]string{"bmc1": "on", "bmc2": "off"}, failed: []string{"bmc3"},
		},
		{
			name: "ipmitool control failed", backend: ipmi.BackendIpmitool, action: ipmi.ActionOff,
			out: "bmc1: Set Chassis Power Control to Down/Off failed: Command not supported in present state\n" +
				"bmc2: Chassis Power Control: Down/Off\nbmc3: Chassis Power Control: Up/On\n",
			// bmc3 answered for another action.
			ok: map[string]string{"bmc2": "ok"}, failed: []string{"bmc1", "bmc3"},
		},
		{
			name: "ipmitool status", backend: ipmi.BackendIpmitool, action: ipmi.ActionStatus,
			out: "bmc1: Chassis Power is on\nbmc2: Chassis Power is off\nbmc3: exit 1: Error: Unable to establish IPMI v2 / RMCP+ session\n",
			ok:  map[string]string{"bmc1": "on", "bmc2": "off"}, failed: []string{"bmc3"},
		},
		{
			name: "ipmitool missing", backend: ipmi.BackendIpmitool, action: ipmi.ActionOff,
			out:    "bmc1: exit 127: sh: 1: /usr/bin/ipmitool: No such file or directory\nbmc2: exit 127: sh: 1: /usr/bin/ipmitool: No such file or directory\n",
			failed: []string{"bmc1", "bmc2", "bmc3"},
		},
		{
			name: "ipmipower missing", backend: ipmi.BackendIpmipower, action: ipmi.ActionOff,
			stderr: "sh: 1: /usr/sbin/ipmipower: not found\n", exit: 127,
			failed: []string{"bmc1", "bmc2", "bmc3"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, runner := backend(t, v1alpha1.IPMISpec{Backend: tc.backend}, tc.out)
			runner.stderr, runner.exit = tc.stderr, tc.exit
			statuses, err := b.Power(context.Background(), tc.action, nodeset.MustParse("bmc[1-3]"))
			if err != nil {
				t.Fatalf("Power failed: %v", err)
			}
			for bmc, state := range tc.ok {
				if s := statusOf(t, statuses, bmc); s.Err != "" || s.State != state {
					t.Errorf("%s = %+v, want state %q and no error", bmc, s, state)
				}
			}
			for _, bmc := range tc.failed {
				s := statusOf(t, statuses, bmc)
				if s.Err == "" || s.Cause == nil {
					t.Errorf("%s = %+v, want a failure", bmc, s)
				}
			}
			if tc.exit == 127 {
				if s := statusOf(t, statuses, "bmc1"); !strings.Contains(s.Err, "not found") {
					t.Errorf("the backend's own message is lost: %+v", s)
				}
			}
		})
	}
}

// A processor whose name begins with another's was answered for by the
// other's line.
func TestAnswersAreMatchedToTheWholeName(t *testing.T) {
	t.Parallel()

	b, _ := backend(t, v1alpha1.IPMISpec{}, "bmc10: ok\n")
	statuses, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc[1,10]"))
	if err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if s := statusOf(t, statuses, "bmc1"); s.Err == "" {
		t.Errorf("bmc1 was taken as answered by bmc10's line: %+v", s)
	}
	if s := statusOf(t, statuses, "bmc10"); s.Err != "" {
		t.Errorf("bmc10 = %+v, want ok", s)
	}
}

// The FreeIPMI configuration file was written without quoting, so a # cut
// the password short and a space, quote or backslash broke the file.
func TestTheAccountIsQuotedForFreeIPMI(t *testing.T) {
	t.Parallel()

	b, runner := backend(t, v1alpha1.IPMISpec{}, "bmc1: ok\n")
	b.Username = `ad min`
	b.Password = `ab#c d"e\f`
	if _, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc1")); err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	want := "username \"ad min\"\npassword \"ab\\#c d\\\"e\\\\f\"\n"
	if runner.payload != want {
		t.Errorf("configuration file = %q, want %q", runner.payload, want)
	}

	// A line break cannot be written into either file, so it is refused
	// before anything is sent.
	for _, spec := range []v1alpha1.IPMISpec{{Backend: ipmi.BackendIpmipower}, {Backend: ipmi.BackendIpmitool}} {
		b, runner := backend(t, spec, "")
		b.Password = "two\nlines"
		_, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc1"))
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("%s: a password with a line break: exit code %d, want %d (%v)", spec.Backend, got, exitcode.Usage, err)
		}
		if runner.script != "" {
			t.Errorf("%s: something was sent for a password with a line break", spec.Backend)
		}
	}
}

// A rejected argument is a usage error, not a host that could not be
// reached.
func TestArgumentErrorsAreUsageErrors(t *testing.T) {
	t.Parallel()

	b, runner := backend(t, v1alpha1.IPMISpec{}, "")
	for name, run := range map[string]func() error{
		"unknown action": func() error {
			_, err := b.Power(context.Background(), "offf", nodeset.MustParse("bmc1"))
			return err
		},
		"empty set": func() error {
			_, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.New())
			return err
		},
	} {
		if got := exitcode.From(run()); got != exitcode.Usage {
			t.Errorf("%s: exit code %d, want %d", name, got, exitcode.Usage)
		}
	}
	if runner.script != "" {
		t.Error("something was sent for a rejected request")
	}
}

// ipmitool is run once per processor in a loop, so one timeout for the
// whole loop ran out after a few dozen unreachable processors.
func TestIpmitoolTimeoutScalesWithTheSet(t *testing.T) {
	t.Parallel()

	b, runner := backend(t, v1alpha1.IPMISpec{Backend: ipmi.BackendIpmitool}, "")
	b.Spec.Timeout = v1alpha1.Duration(time.Minute)
	if _, err := b.Power(context.Background(), ipmi.ActionOff, nodeset.MustParse("bmc[1-40]")); err != nil {
		t.Fatalf("Power failed: %v", err)
	}
	if runner.timeout < 40*20*time.Second {
		t.Errorf("the loop over 40 processors is bounded by %s", runner.timeout)
	}
	if !strings.Contains(runner.script, "timeout ") {
		t.Errorf("each ipmitool run is not bounded on its own:\n%s", runner.script)
	}
}
