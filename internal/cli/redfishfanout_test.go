// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout/fanouttest"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// processorOf names the node a request to a processor is for.
func processorOf(req *http.Request) string {
	node, _, _ := strings.Cut(req.URL.Hostname(), ".")
	return node
}

// bmc.redfish.maxConcurrent bounds the requests in flight to the processors.
// Each request is held until one more than the limit are under way, which
// never happens while the limit is kept, so exactly the limit run at once.
func TestTheRedfishFanOutKeepsToItsLimit(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "s3cret")
	calls := &fanouttest.InFlight{Hold: 3}
	fakeRedfish(t, calls.RoundTripper(roundTrip(func(req *http.Request) (*http.Response, error) {
		return answer(req, http.StatusOK, system), nil
	})).RoundTrip)

	h, err := run(t, harnessOptions{recorder: ipmiOK()},
		append(noSlurm, "--set", "bmc.redfish.maxConcurrent=2", "--set", "bmc.order=[redfish]",
			"bmc", "power", "status", "-n", "exe[0001-0005]")...)
	if err != nil {
		t.Fatalf("bmc power status failed: %v\n%s", err, h.errOut)
	}
	if got := calls.Peak(); got != 2 {
		t.Errorf("%d requests were in flight at once, want 2", got)
	}
	if got := calls.Started(); got < 5 {
		t.Errorf("%d requests were sent, want one for each of the 5 processors at least", got)
	}
}

// A panic while a request went to one processor ended the process: the
// Redfish fan-outs of bmc power and provision had no recover. It is that
// node's failure now; the others are reported, and the command fails.
func TestAPanicOnOneProcessorFailsOnlyThatNode(t *testing.T) {
	t.Run("bmc power", func(t *testing.T) {
		isolateHome(t)
		t.Setenv("BMC_PASSWORD", "s3cret")
		fakeRedfish(t, func(req *http.Request) (*http.Response, error) {
			if processorOf(req) == "exe0002" {
				panic("index out of range [3] with length 3")
			}
			return answer(req, http.StatusOK, system), nil
		})
		h, err := run(t, harnessOptions{recorder: ipmiOK()},
			append(noSlurm, "--set", "bmc.order=[redfish]", "-o", "json", "bmc", "power", "status", "-n", "exe[0001-0003]")...)
		wantCode(t, err, exitcode.TargetFailed)
		for _, row := range jsonRows(t, h) {
			switch row["node"] {
			case "exe0002":
				if msg, _ := row["error"].(string); !strings.Contains(msg, "panicked") {
					t.Errorf("exe0002 = %v, want it failed with the panic", row)
				}
			default:
				if row["state"] != "On" || row["error"] != nil {
					t.Errorf("%v = %v, want it On", row["node"], row)
				}
			}
		}
		if !strings.Contains(h.errOut.String(), "panic while working on exe0002") {
			t.Errorf("the stack of the panic was not written to standard error:\n%s", h.errOut)
		}
	})

	t.Run("provision reinstall", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
		h.bmcs.panics["exe0002"] = "Pxe"
		_, err := h.run(t, harnessOptions{}, "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
		wantCode(t, err, exitcode.TargetFailed)
		for _, node := range []string{"exe0001", "exe0003"} {
			if got, want := strings.Join(h.bmcs.of(node), ", "), "boot once, clear"; got != want {
				t.Errorf("%s: sent %s, want %s", node, got, want)
			}
		}
		if err == nil || !strings.Contains(err.Error(), "exe0002") || !strings.Contains(err.Error(), "panicked") {
			t.Errorf("error = %v, want it to say clusterctl panicked on exe0002", err)
		}
		if !h.keysKept(t) {
			t.Error("the host keys were forgotten although nothing was reset")
		}
	})

	t.Run("provision status", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
		h.bmcs.panics["exe0002"] = "GET"
		out, err := h.run(t, harnessOptions{}, "provision", "status", "-n", "exe[0001-0003]")
		if exitcode.From(err) == exitcode.OK {
			t.Fatalf("provision status succeeded although exe0002 panicked:\n%s", out.out)
		}
		if !strings.Contains(out.out.String(), "exe0003") {
			t.Errorf("exe0003 is not reported:\n%s", out.out)
		}
	})
}

// bmc power and provision reinstall each had a Redfish fan-out, and they
// classified a processor that presented another certificate differently:
// provision as a request that never reached it, bmc power not, where only
// an extra check kept it from falling back to IPMI. The certificate is
// refused in the handshake, before the request is written, so the one
// fan-out counts it as never sent. Both commands exit 3, neither sends the
// account over another transport, and provision has no override to clear.
func TestAChangedCertificateIsOneFailureInEveryCommand(t *testing.T) {
	t.Run("bmc power", func(t *testing.T) {
		isolateHome(t)
		t.Setenv("BMC_PASSWORD", "s3cret")
		fakeRedfish(t, func(req *http.Request) (*http.Response, error) {
			if processorOf(req) == "exe0001" {
				return nil, &redfish.PinMismatchError{Host: req.URL.Hostname(), Recorded: "SHA256:old", Seen: "SHA256:new"}
			}
			return answer(req, http.StatusOK, system), nil
		})
		h, err := run(t, harnessOptions{recorder: ipmiOK()},
			append(noSlurm, "-o", "json", "bmc", "power", "off", "-y", "-n", "exe[0001-0002]")...)
		wantCode(t, err, exitcode.Transport)
		if calls := ipmiCalls(h); len(calls) != 0 {
			t.Errorf("exe0001 fell back to IPMI: %v", h.recorder.Commands())
		}
		for _, row := range jsonRows(t, h) {
			if row["node"] != "exe0001" {
				continue
			}
			if msg, _ := row["error"].(string); !strings.Contains(msg, "certificate") {
				t.Errorf("exe0001: error %q, want it to name the certificate", msg)
			}
		}
	})

	t.Run("provision reinstall", func(t *testing.T) {
		h := newReinstallHost(t, pxeOptions{inventory: threeNodes})
		h.bmcs.pinned["exe0002"] = true
		out, err := h.run(t, harnessOptions{}, "-o", "json", "provision", "reinstall", "-n", "exe[0001-0003]", "-y")
		wantCode(t, err, exitcode.Transport)
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("error = %v, want it to name the certificate", err)
		}
		if got := h.bmcs.of("exe0002"); len(got) != 0 {
			t.Errorf("exe0002: sent %q", got)
		}
		if !strings.Contains(out.out.String(), `"bootOnce": "unreachable"`) {
			t.Errorf("exe0002 is not reported as unreachable:\n%s", out.out)
		}
	})
}
