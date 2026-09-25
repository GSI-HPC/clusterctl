// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// noSlurm turns the Slurm job check off, which these tests are not about.
var noSlurm = []string{"--set", "safety.slurmAware=false"}

// ipmiCalls returns the recorded IPMI runs.
func ipmiCalls(h *harness) []transport.Call {
	var out []transport.Call
	for _, c := range h.recorder.Calls() {
		if strings.Contains(c.Request.Script, "--hostname") || strings.Contains(c.Request.Script, "chassis power") {
			out = append(out, c)
		}
	}
	return out
}

// jsonRows decodes the one JSON value a command printed.
func jsonRows(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &rows); err != nil {
		t.Fatalf("the output is not one JSON array: %v\n%s", err, h.out)
	}
	return rows
}

// A misspelt action went to Slurm, and over IPMI to the credential, and then
// exited 3 as though a host could not be reached; the dry run approved it.
func TestBMCPowerChecksTheActionFirst(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	for _, args := range [][]string{
		{"bmc", "power", "offf", "--ipmi", "-y", "-n", "exe0001"},
		{"bmc", "power", "offf", "-y", "-n", "exe0001"},
		{"--dry-run", "bmc", "power", "offf", "-n", "exe0001"},
		// reboot was accepted over Redfish only, and is in no list.
		{"bmc", "power", "reboot", "-y", "-n", "exe0001"},
	} {
		h, err := run(t, harnessOptions{}, args...)
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("%v: exit code %d, want %d (%v)", args, got, exitcode.Usage, err)
		}
		if calls := h.recorder.Calls(); len(calls) != 0 {
			t.Errorf("%v: sent %d commands before refusing the action: %v", args, len(calls), h.recorder.Commands())
		}
	}
}

// An IPMI backend that timeout(1) stopped after one answer made the command
// exit 0 with one row for four nodes.
func TestBMCPowerOverIPMIReportsEveryNode(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	recorder := &transport.Recorder{Reply: func(target transport.Target, req transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: target, Stdout: "exe0001.mgmt.hpc.example.org: ok\n", ExitCode: 124}, nil
	}}
	h, err := run(t, harnessOptions{recorder: recorder},
		append(noSlurm, "-o", "json", "bmc", "power", "off", "--ipmi", "-y", "-n", "exe[0001-0004]")...)
	wantCode(t, err, exitcode.Transport)
	rows := jsonRows(t, h)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4:\n%s", len(rows), h.out)
	}
	for _, row := range rows[1:] {
		if row["error"] == nil {
			t.Errorf("%v was never reported but has no error", row["node"])
		}
	}
}

// Only a power-on was batched; a cycle of a rack drew its inrush current at
// once. Batch 8 over ten nodes is sent as 5 and 5, as the manual says.
func TestBMCPowerCycleIsBatched(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	h, err := run(t, harnessOptions{recorder: ipmiOK()},
		append(noSlurm, "bmc", "power", "cycle", "--ipmi", "--stagger", "1ms", "-y", "-n", "exe[0001-0010]")...)
	if err != nil {
		t.Fatalf("bmc power cycle failed: %v\n%s", err, h.errOut)
	}
	calls := ipmiCalls(h)
	if len(calls) != 2 {
		t.Fatalf("got %d IPMI runs, want 2: %v", len(calls), h.recorder.Commands())
	}
	for _, c := range calls {
		if got := len(ipmiRequestHosts(c.Request)); got != 5 {
			t.Errorf("a batch of %d, want 5: %s", got, c.Command)
		}
	}
}

// A power-on stopped at the first batch with a failure and never mentioned
// the rest; each batch printed its own JSON array; an explicit zero was
// taken for no flag.
func TestBMCPowerOnReportsEveryBatch(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	recorder := &transport.Recorder{Reply: ipmiAnswer(func(bmc string) string {
		if strings.HasPrefix(bmc, "exe0002.") {
			return "connection timeout"
		}
		return "ok"
	})}
	h, err := run(t, harnessOptions{recorder: recorder},
		append(noSlurm, "-o", "json", "bmc", "power", "on", "--ipmi", "--batch", "3", "--stagger", "1ms", "-y", "-n", "exe[1-10]")...)
	if err == nil {
		t.Fatal("a failed batch was not reported")
	}
	if !strings.Contains(err.Error(), "1 of 10 service processors failed, 7 not tried: exe[0004-0010]") {
		t.Errorf("error = %v, want it to count the untried nodes and name them", err)
	}
	rows := jsonRows(t, h)
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10:\n%s", len(rows), h.out)
	}
	if got := rows[9]["state"]; got != "not tried" {
		t.Errorf("exe0010 state = %v, want not tried", got)
	}

	// Without failures the whole run is one JSON value.
	h, err = run(t, harnessOptions{recorder: ipmiOK()},
		append(noSlurm, "-o", "json", "bmc", "power", "on", "--ipmi", "--batch", "3", "--stagger", "0", "-y", "-n", "exe[1-10]")...)
	if err != nil {
		t.Fatalf("power on failed: %v", err)
	}
	if rows := jsonRows(t, h); len(rows) != 10 {
		t.Errorf("got %d rows, want 10", len(rows))
	}
	// --stagger 0 means no pause, not the configured 5s.
	if strings.Contains(h.errOut.String(), "waiting") {
		t.Errorf("--stagger 0 still paused:\n%s", h.errOut)
	}
	if got := len(ipmiCalls(h)); got != 4 {
		t.Errorf("got %d batches, want 4", got)
	}

	// An explicit --batch below 1 is refused like safety.powerOnBatch, not
	// replaced by the configuration and not read as the whole set at once.
	h, err = run(t, harnessOptions{recorder: ipmiOK()},
		append(noSlurm, "bmc", "power", "on", "--ipmi", "--batch", "0", "-y", "-n", "exe[1-10]")...)
	if got := exitcode.From(err); got != exitcode.Usage {
		t.Errorf("--batch 0: exit code %d, want %d (%v)", got, exitcode.Usage, err)
	}
	if got := len(ipmiCalls(h)); got != 0 {
		t.Errorf("--batch 0: %d batches were sent, want none", got)
	}
}

// A power-on interrupted during a batch sends no more batches, and says of
// their nodes that they were not sent, as for an interrupt during a pause:
// the batch it cut short failed, but the interrupt is what left the rest
// out. The command exits 130.
func TestBMCPowerOnInterruptedDuringABatch(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := ipmiAnswer(func(bmc string) string {
		if strings.HasPrefix(bmc, "exe0004.") {
			cancel()
			return "connection timeout"
		}
		return "ok"
	})
	h, err := run(t, harnessOptions{ctx: ctx, recorder: &transport.Recorder{Reply: answer}},
		append(noSlurm, "-o", "json", "bmc", "power", "on", "--ipmi", "--batch", "3", "--stagger", "1ms", "-y", "-n", "exe[1-9]")...)
	wantCode(t, err, exitcode.Interrupted)
	rows := jsonRows(t, h)
	if len(rows) != 9 {
		t.Fatalf("got %d rows, want 9:\n%s", len(rows), h.out)
	}
	for _, row := range rows[6:] {
		if row["state"] != "not sent" {
			t.Errorf("%v: state %v, want not sent", row["node"], row["state"])
		}
	}
	if got := len(ipmiCalls(h)); got != 2 {
		t.Errorf("got %d batches sent, want 2", got)
	}
}

// One backend was built from the first node's vendor profile, so every
// processor in the set was offered that vendor's account.
func TestBMCPowerGroupsTheSetByAccount(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	t.Setenv("V1_PASSWORD", "v1pass")
	site := exampleWith(t, "site.yaml", func(s string) string {
		s = strings.Replace(s, "  credentials:\n",
			"  credentials:\n    bmc-v1:\n      username: admin1\n      password:\n        fromEnv: V1_PASSWORD\n", 1)
		s = strings.Replace(s, "    vendors:\n", "    vendors:\n      vendor1:\n        credential: bmc-v1\n        order: [ipmi]\n", 1)
		return strings.Replace(s, "backend: ipmipower", "backend: ipmitool", 1)
	})
	args := append(noSlurm, "--set", "bmc.order=[ipmi]", "bmc", "power", "off", "--force", "-y", "-n", "dbm01,exe0005")

	h, err := run(t, harnessOptions{config: []string{site}, recorder: ipmiOK()}, args...)
	if err != nil {
		t.Fatalf("bmc power failed: %v\n%s", err, h.errOut)
	}
	calls := ipmiCalls(h)
	if len(calls) != 2 {
		t.Fatalf("got %d IPMI runs, want one per account: %v", len(calls), h.recorder.Commands())
	}
	for _, c := range calls {
		hosts := strings.Join(ipmiRequestHosts(c.Request), ",")
		switch {
		case strings.Contains(c.Command, "admin1"):
			if hosts != "dbm01.mgmt.hpc.example.org" {
				t.Errorf("vendor1's account went to %s", hosts)
			}
		default:
			if hosts != "exe0005.mgmt.hpc.example.org" {
				t.Errorf("the site's account went to %s", hosts)
			}
		}
	}

	// The preview names each group.
	h, err = run(t, harnessOptions{config: []string{site}}, append([]string{"--dry-run"}, args...)...)
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if out := h.errOut.String() + h.out.String(); !strings.Contains(out, "bmc-v1") || !strings.Contains(out, "exe0005") {
		t.Errorf("the preview does not show the groups:\n%s", out)
	}
}

// A misspelt transport chose Redfish; a node without a Redfish service
// failed instead of falling back to IPMI; bmc power status ignored the order.
func TestBMCOrderIsCheckedAndFallsBack(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")

	_, err := run(t, harnessOptions{}, "--dry-run", "--set", "bmc.order=[impi]", "bmc", "power", "off", "-n", "exe0001")
	if got := exitcode.From(err); got != exitcode.Usage {
		t.Errorf("bmc.order=[impi]: exit code %d, want %d (%v)", got, exitcode.Usage, err)
	}

	// Nothing listens for Redfish on 127.0.0.1, so the connection is
	// refused and the request provably never reached the processor.
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0001\n      bmcAddress: 127.0.0.1\n"
	})
	for _, args := range [][]string{
		{"bmc", "status", "-n", "exe0001"},
		{"bmc", "power", "status", "-n", "exe0001"},
		{"bmc", "power", "off", "-y", "-n", "exe0001"},
	} {
		h, err := run(t, harnessOptions{config: []string{inventory}, recorder: ipmiOK()}, append(noSlurm, args...)...)
		if err != nil {
			t.Errorf("%v: failed instead of falling back to IPMI: %v\n%s%s", args, err, h.out, h.errOut)
			continue
		}
		if got := ipmiHosts(h); len(got) != 1 || got[0] != "127.0.0.1" {
			t.Errorf("%v: IPMI runs named %v, want 127.0.0.1", args, got)
		}
	}

	// bmc power status follows the order.
	h, err := run(t, harnessOptions{recorder: ipmiOK()}, "--set", "bmc.order=[ipmi]", "bmc", "power", "status", "-n", "exe0001")
	if err != nil {
		t.Fatalf("bmc power status failed: %v", err)
	}
	if len(ipmiCalls(h)) != 1 {
		t.Errorf("bmc power status did not use IPMI first: %v", h.recorder.Commands())
	}
}

// A missing credential was one row among the results and exit 1, where the
// IPMI path exited 2; a processor that refused the connection exited 1.
func TestBMCFailuresKeepTheirExitCode(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "")
	if err := os.Unsetenv("BMC_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"bmc", "power", "off", "-y", "-n", "exe0001"},
		{"bmc", "status", "-n", "exe0001"},
		{"-o", "json", "bmc", "redfish", "info", "-n", "exe0001"},
		{"bmc", "boot", "show", "-n", "exe0001"},
	} {
		h, err := run(t, harnessOptions{}, append(noSlurm, args...)...)
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("%v without a password: exit code %d, want %d (%v)\n%s", args, got, exitcode.Usage, err, h.out)
		}
	}

	t.Setenv("BMC_PASSWORD", "s3cret")
	inventory := exampleWith(t, "inventory.yaml", func(s string) string {
		return s + "    - nodes: exe0001\n      bmcAddress: 127.0.0.1\n"
	})
	redfishOnly := []string{"--set", "bmc.order=[redfish]"}
	for _, args := range [][]string{
		{"bmc", "status", "-n", "exe0001"},
		{"bmc", "power", "off", "-y", "-n", "exe0001"},
		{"bmc", "boot", "set", "Pxe", "-y", "-n", "exe0001"},
		{"bmc", "boot", "show", "-n", "exe0001"},
		{"bmc", "redfish", "get", "/redfish/v1", "-n", "exe0001"},
	} {
		_, err := run(t, harnessOptions{config: []string{inventory}}, append(append(noSlurm, redfishOnly...), args...)...)
		if got := exitcode.From(err); got != exitcode.Transport {
			t.Errorf("%v against a refused connection: exit code %d, want %d (%v)", args, got, exitcode.Transport, err)
		}
	}

	// bmc redfish info names every node in its object, the failed ones with
	// their error.
	h, err := run(t, harnessOptions{config: []string{inventory}},
		append(redfishOnly, "-o", "json", "bmc", "redfish", "info", "-n", "exe[0001-0002]")...)
	if got := exitcode.From(err); got != exitcode.Transport {
		t.Errorf("redfish info: exit code %d, want %d (%v)", got, exitcode.Transport, err)
	}
	var object map[string]map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &object); err != nil {
		t.Fatalf("redfish info printed no JSON object: %v\n%s", err, h.out)
	}
	if object["exe0001"]["error"] == nil {
		t.Errorf("exe0001 is missing its error: %v", object)
	}
}

// An interrupt before the fan-out reported every node as failed with exit 1,
// although nothing could have been sent.
func TestBMCInterruptBeforeSendingExits130(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")
	ctx, cancel := context.WithCancel(context.Background())
	// The interrupt arrives while the confirmation is being given.
	recorder := &transport.Recorder{Reply: func(target transport.Target, _ transport.Request) (*transport.Result, error) {
		cancel()
		return &transport.Result{Target: target, Stdout: "exe0001 idle\nexe0002 idle\nexe0003 idle\n"}, nil
	}}
	defer cancel()
	h, err := run(t, harnessOptions{ctx: ctx, recorder: recorder},
		"-o", "json", "bmc", "power", "off", "-y", "-n", "exe[0001-0003]")
	wantCode(t, err, exitcode.Interrupted)
	if strings.Contains(h.out.String(), "state") {
		for _, row := range jsonRows(t, h) {
			if row["state"] != "not sent" {
				t.Errorf("%v: state %v, want not sent", row["node"], row["state"])
			}
		}
	}
}

// A node tried over Redfish and then over IPMI is one node: a counter of
// the step reaches its total once every node has its last answer, whichever
// transport gave it, and a node that fell back is not counted twice, nor a
// node sent over IPMI alone left out. The failure over IPMI carries the one
// over Redfish, as the table does.
func TestBMCPowerReportsEachNodeOnceAcrossItsTransports(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")
	// exe0002's processor refuses the connection, which falls back to IPMI
	// even for an action; exe0003's answers with an error, which falls
	// back only for a read.
	fakeRedfish(t, func(req *http.Request) (*http.Response, error) {
		switch processorOf(req) {
		case "exe0002":
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		case "exe0003":
			return answer(req, http.StatusInternalServerError, `{"error":{"message":"busy"}}`), nil
		}
		return answer(req, http.StatusOK, system), nil
	})
	// ipmiFailing answers every processor over IPMI with state, but
	// exe0002's with a timeout.
	ipmiFailing := func(state string) *transport.Recorder {
		return &transport.Recorder{Reply: ipmiAnswer(func(bmc string) string {
			if strings.HasPrefix(bmc, "exe0002.") {
				return "connection timeout"
			}
			return state
		})}
	}

	for _, tc := range []struct {
		name     string
		recorder *transport.Recorder
		args     []string
		want     string
	}{
		{"bmc status over Redfish alone", ipmiOK(),
			[]string{"--set", "bmc.order=[redfish]", "bmc", "status", "-n", "exe[0001,0003-0004]"}, `
step power status total=3 [fold]: failed (target): 1 of 3 service processors failed
  target exe0003: failed (target): {}: Internal Server Error: busy
  target exe[0001,0004]: ok
`},
		{"a read that falls back to IPMI and fails there too", ipmiFailing("on"),
			[]string{"bmc", "status", "-n", "exe[0001-0003]"}, `
step power status total=3 [fold]: failed (target): 1 of 3 service processors failed
  target exe0002: failed (target): connection timeout; before that, Redfish failed: {}: dial tcp: connection refused
  target exe[0001,0003]: ok
`},
		{"an action that falls back only where it was never sent", ipmiOK(),
			[]string{"bmc", "power", "off", "-y", "-n", "exe[0001-0003]"}, `
step power off total=3 [fold]: failed (target): 1 of 3 service processors failed
  target exe0003: failed (target): {}: Internal Server Error: busy
  target exe[0001-0002]: ok
`},
		{"IPMI alone", ipmiFailing("ok"),
			[]string{"bmc", "power", "off", "--ipmi", "-y", "-n", "exe[0001-0003]"}, `
step power off total=3 [fold]: failed (target): 1 of 3 service processors failed
  target exe0002: failed (target): connection timeout
  target exe[0001,0003]: ok
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, tree := watch(t)
			_, err := run(t, harnessOptions{ctx: ctx, recorder: tc.recorder}, append(noSlurm, tc.args...)...)
			wantCode(t, err, exitcode.TargetFailed)
			if got := tree(); got != tc.want[1:] {
				t.Errorf("progress:\n%s\nwant:\n%s", got, tc.want[1:])
			}
		})
	}
}

// A Redfish target ends as its processor answers, before the request gives
// its place to the next, so a display counts no more nodes running than
// bmc.redfish.maxConcurrent, rather than every node until the slowest
// processor has answered.
func TestBMCPowerEndsEachNodeAsItsProcessorAnswers(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")
	fakeRedfish(t, func(req *http.Request) (*http.Response, error) {
		return answer(req, http.StatusOK, system), nil
	})
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	_, err := run(t, harnessOptions{ctx: progress.WithBus(context.Background(), bus), recorder: ipmiOK()},
		append(noSlurm, "--set", "bmc.redfish.maxConcurrent=2", "bmc", "power", "off", "-y", "-n", "exe[0001-0006]")...)
	if err != nil {
		t.Fatalf("bmc power off failed: %v", err)
	}
	bus.Close()
	events := c.Events()
	progresstest.Check(t, events)

	running := map[progress.SpanID]bool{}
	peak := 0
	for _, e := range events {
		if e.Kind != progress.KindTarget {
			continue
		}
		switch e.Type {
		case progress.TypeRun:
			running[e.Span] = true
			peak = max(peak, len(running))
		case progress.TypeEnd:
			delete(running, e.Span)
		}
	}
	if peak > 2 {
		t.Errorf("%d nodes were running at once, want the limit of 2 at most", peak)
	}
}

// fakeStagger replaces the pause between two batches for the rest of the
// test: pause is called with its length instead of waiting it out, and
// the pause ends at once unless pause says to wait for the context.
func fakeStagger(t *testing.T, pause func(time.Duration) (wait bool)) {
	t.Helper()
	previous := staggerAfter
	t.Cleanup(func() { staggerAfter = previous })
	staggerAfter = func(d time.Duration) <-chan time.Time {
		over := make(chan time.Time, 1)
		if !pause(d) {
			over <- time.Time{}
		}
		return over
	}
}

// A power-on in batches is one step over the whole set, with every batch
// announced before the first is sent and the pause between two a wait of
// its own: a counter reaches its total whether a batch failed, which
// leaves the later ones not tried, or an interrupt came in a pause, which
// leaves them not sent. The notes on standard error and the rows are what
// they were.
func TestBMCPowerReportsItsBatches(t *testing.T) {
	t.Setenv("BMC_PASSWORD", "s3cret")
	for _, tc := range []struct {
		name      string
		nodes     string
		interrupt bool
		code      int
		notes     []string
		rows      map[string]string
		want      string
	}{
		{"a batch fails", "exe[1-10]", false, exitcode.TargetFailed,
			[]string{
				"powering on exe[0001-0003] (1 of 4)\nwaiting 5s before the next batch\npowering on exe[0004-0006] (2 of 4)\n",
			},
			map[string]string{"exe0001": "ok", "exe0005": "unknown", "exe0007": "not tried", "exe0010": "not tried"}, `
step power on total=10 [fold]: failed (target): 1 of 3 service processors failed
  batch 1/4 node=exe[0001-0003] batch=1/4 total=3: ok
    target exe[0001-0003]: ok
  batch 2/4 node=exe[0004-0006] batch=2/4 total=3: failed (target): 1 of 3 service processors failed
    target exe0005: failed (target): connection timeout
    target exe[0004,0006]: ok
  batch 3/4 node=exe[0007-0008] batch=3/4 total=2: skipped: not tried: an earlier batch failed
  batch 4/4 node=exe[0009-0010] batch=4/4 total=2: skipped: not tried: an earlier batch failed
  wait stagger timeout=5s: ok
`},
		{"an interrupt in a pause", "exe[1-6]", true, exitcode.Interrupted,
			[]string{"powering on exe[0001-0003] (1 of 2)\nwaiting 5s before the next batch\n"},
			map[string]string{"exe0003": "ok", "exe0004": "not sent", "exe0006": "not sent"}, `
step power on total=6 [fold]: canceled (canceled): context canceled
  batch 1/2 node=exe[0001-0003] batch=1/2 total=3: ok
    target exe[0001-0003]: ok
  batch 2/2 node=exe[0004-0006] batch=2/2 total=3: canceled (canceled): context canceled
  wait stagger timeout=5s: canceled (canceled): context canceled
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watched, tree := watch(t)
			ctx, cancel := context.WithCancel(watched)
			defer cancel()
			var pauses []time.Duration
			fakeStagger(t, func(d time.Duration) bool {
				pauses = append(pauses, d)
				if tc.interrupt {
					cancel()
				}
				return tc.interrupt
			})
			recorder := &transport.Recorder{Reply: ipmiAnswer(func(bmc string) string {
				if strings.HasPrefix(bmc, "exe0005.") {
					return "connection timeout"
				}
				return "ok"
			})}

			h, err := run(t, harnessOptions{ctx: ctx, recorder: recorder},
				append(noSlurm, "-o", "json", "bmc", "power", "on", "--ipmi", "--batch", "3", "-y", "-n", tc.nodes)...)
			wantCode(t, err, tc.code)
			if len(pauses) != 1 || pauses[0] != 5*time.Second {
				t.Errorf("paused %v, want once for safety.powerOnStagger, 5s", pauses)
			}
			for _, note := range tc.notes {
				if !strings.Contains(h.errOut.String(), note) {
					t.Errorf("standard error does not say %q:\n%s", note, h.errOut)
				}
			}
			states := map[string]string{}
			for _, row := range jsonRows(t, h) {
				states[row["node"].(string)], _ = row["state"].(string)
			}
			if want, _ := nodeset.Parse(tc.nodes); len(states) != want.Len() {
				t.Errorf("%d rows, want one for each of the %d nodes:\n%s", len(states), want.Len(), h.out)
			}
			for node, want := range tc.rows {
				if states[node] != want {
					t.Errorf("%s: state %q, want %q", node, states[node], want)
				}
			}
			if got := tree(); got != tc.want[1:] {
				t.Errorf("progress:\n%s\nwant:\n%s", got, tc.want[1:])
			}
		})
	}
}
