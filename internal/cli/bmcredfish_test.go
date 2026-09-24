// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// answer is a JSON reply with a status.
func answer(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// system is the computer system a fake processor describes.
const system = `{"PowerState":"On","Actions":{"#ComputerSystem.Reset":{"target":"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset"}}}`

// fakeRedfish puts a fake behind every Redfish client the commands build.
func fakeRedfish(t *testing.T, rt roundTrip) {
	t.Helper()
	previous := redfishClientFor
	t.Cleanup(func() { redfishClientFor = previous })
	redfishClientFor = func(a *app.App, ctx context.Context, node string) (*redfish.Client, error) {
		c, err := a.RedfishClient(ctx, node)
		if err != nil {
			return nil, err
		}
		c.Transport = rt
		return c, nil
	}
}

// After Ctrl-C a reset that was under way and one never sent both read
// "interrupt signal received" and counted as failed, so re-running for the
// failed nodes power-cycled the first a second time.
func TestBMCInterruptedResetIsReportedAsUnknown(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fakeRedfish(t, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			// The interrupt arrives while the reset is under way.
			cancel()
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return answer(req, http.StatusOK, system), nil
	})

	h, err := run(t, harnessOptions{ctx: ctx},
		append(noSlurm, "--set", "bmc.redfish.maxConcurrent=1", "-o", "json",
			"bmc", "power", "cycle", "--batch", "3", "-y", "-n", "exe[0001-0003]")...)
	if got := exitcode.From(err); got != exitcode.Interrupted {
		t.Errorf("exit code %d, want %d (%v)", got, exitcode.Interrupted, err)
	}
	states := map[string]any{}
	for _, row := range jsonRows(t, h) {
		states[row["node"].(string)] = row["state"]
	}
	want := map[string]any{"exe0001": "outcome unknown", "exe0002": "not sent", "exe0003": "not sent"}
	for node, state := range want {
		if states[node] != state {
			t.Errorf("%s: state %v, want %v", node, states[node], state)
		}
	}
	if err != nil && !strings.Contains(err.Error(), "2 not sent: exe[0002-0003]") {
		t.Errorf("error = %v, want it to name what was not sent", err)
	}
}

// An action that reached the processor is never sent again over IPMI, even
// when it failed; a status is.
func TestBMCActionThatReachedTheProcessorIsNotSentAgain(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")
	fakeRedfish(t, func(req *http.Request) (*http.Response, error) {
		return answer(req, http.StatusInternalServerError, `{"error":{"message":"busy"}}`), nil
	})

	h, err := run(t, harnessOptions{recorder: ipmiOK()}, append(noSlurm, "bmc", "power", "off", "-y", "-n", "exe0001")...)
	if got := exitcode.From(err); got != exitcode.TargetFailed {
		t.Errorf("exit code %d, want %d (%v)", got, exitcode.TargetFailed, err)
	}
	if calls := ipmiCalls(h); len(calls) != 0 {
		t.Errorf("the action was sent again over IPMI: %v", h.recorder.Commands())
	}

	h, err = run(t, harnessOptions{recorder: ipmiOK()}, "bmc", "status", "-n", "exe0001")
	if err != nil {
		t.Fatalf("bmc status did not fall back to IPMI: %v\n%s", err, h.out)
	}
	if calls := ipmiCalls(h); len(calls) != 1 {
		t.Errorf("bmc status: got %d IPMI runs, want 1", len(calls))
	}
	if !strings.Contains(h.errOut.String(), "trying IPMI") {
		t.Errorf("the fallback is not said:\n%s", h.errOut)
	}
}
