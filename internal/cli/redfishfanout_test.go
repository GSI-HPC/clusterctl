// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"net/http"
	"slices"
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

// bmc.redfish.maxConcurrent bounds the requests in flight to the processors,
// and --fanout lowers it where it is lower but never raises it: --fanout 1
// asks one processor at a time, in bmc power as in the other Redfish
// commands, while --fanout 4, and fanout.max from the Site document, --set
// or the environment, leave it at 2. Each request is held until one more
// than the limit are under way, which never happens while the limit is
// kept, so exactly the limit run at once.
func TestTheRedfishFanOutKeepsToItsLimit(t *testing.T) {
	siteFanout := exampleWith(t, "site.yaml", func(s string) string {
		return strings.Replace(s, "  fanout:\n    max: 24\n", "  fanout:\n    max: 1\n", 1)
	})
	powerStatus := []string{"bmc", "power", "status"}
	for _, tc := range []struct {
		name    string
		config  []string
		env     string
		args    []string
		command []string
		want    int
	}{
		{"bmc.redfish.maxConcurrent alone", nil, "", nil, powerStatus, 2},
		{"a lower --fanout", nil, "", []string{"--fanout", "1"}, powerStatus, 1},
		{"a lower --fanout outside bmc power", nil, "", []string{"--fanout", "1"}, []string{"bmc", "redfish", "info"}, 1},
		{"a higher --fanout", nil, "", []string{"--fanout", "4"}, powerStatus, 2},
		{"fanout.max in the Site document", []string{siteFanout}, "", nil, powerStatus, 2},
		{"fanout.max with --set", nil, "", []string{"--set", "fanout.max=1"}, powerStatus, 2},
		{"fanout.max from the environment", nil, "1", nil, powerStatus, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			t.Setenv("BMC_PASSWORD", "s3cret")
			if tc.env != "" {
				t.Setenv("CLUSTERCTL_FANOUT", tc.env)
			}
			calls := &fanouttest.InFlight{Hold: tc.want + 1}
			fakeRedfish(t, calls.RoundTripper(roundTrip(func(req *http.Request) (*http.Response, error) {
				return answer(req, http.StatusOK, system), nil
			})).RoundTrip)

			args := slices.Concat(noSlurm,
				[]string{"--set", "bmc.redfish.maxConcurrent=2", "--set", "bmc.order=[redfish]"},
				tc.args, tc.command, []string{"-n", "exe[0001-0005]"})
			h, err := run(t, harnessOptions{config: tc.config, recorder: ipmiOK()}, args...)
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", strings.Join(tc.command, " "), err, h.errOut)
			}
			if got := calls.Peak(); got != tc.want {
				t.Errorf("%d requests were in flight at once, want %d", got, tc.want)
			}
			if got := calls.Started(); got < 5 {
				t.Errorf("%d requests were sent, want one for each of the 5 processors at least", got)
			}
		})
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
