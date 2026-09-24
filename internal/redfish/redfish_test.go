// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package redfish_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// fakeBMC serves the part of the Redfish surface clusterctl uses.
type fakeBMC struct {
	server   *httptest.Server
	resets   atomic.Int32
	lastPost map[string]any
	patches  []map[string]any
	power    string
}

func newFakeBMC(t *testing.T, resetTypes []string) *fakeBMC {
	t.Helper()
	f := &fakeBMC{power: "On"}

	mux := http.NewServeMux()
	mux.HandleFunc("/redfish/v1/Systems/1", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			allowable := map[string]any{}
			if resetTypes != nil {
				allowable["ResetType@Redfish.AllowableValues"] = toAny(resetTypes)
			}
			allowable["target"] = "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Name":         "System",
				"PowerState":   f.power,
				"Manufacturer": "Vendor",
				"Model":        "Model",
				"BiosVersion":  "1.2.3",
				"Status":       map[string]any{"Health": "OK", "State": "Enabled"},
				"Boot": map[string]any{
					"BootSourceOverrideTarget":                         "None",
					"BootSourceOverrideEnabled":                        "Disabled",
					"BootSourceOverrideTarget@Redfish.AllowableValues": toAny([]string{"None", "Pxe", "Hdd"}),
				},
				"Actions": map[string]any{"#ComputerSystem.Reset": allowable},
			})
		case http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.patches = append(f.patches, body)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", func(w http.ResponseWriter, r *http.Request) {
		f.resets.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&f.lastPost)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/redfish/v1/broken", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "the request failed",
				"@Message.ExtendedInfo": []any{
					map[string]any{"Message": "Unsupported value", "Resolution": "Use a supported value."},
				},
			},
		})
	})

	f.server = httptest.NewTLSServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// client reaches the fake under the name its certificate carries. A host
// with a port is refused, so the port is supplied by the dialer instead.
func (f *fakeBMC) client(t *testing.T) *redfish.Client {
	t.Helper()
	return &redfish.Client{
		Host:      "example.com",
		Username:  "admin",
		Password:  "secret",
		Transport: dialOnly(f.server),
	}
}

// dialOnly returns a transport that trusts the test server and connects to
// it whatever host a request names.
func dialOnly(server *httptest.Server) http.RoundTripper {
	rt := server.Client().Transport.(*http.Transport).Clone()
	addr := server.Listener.Addr().String()
	rt.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return rt
}

func TestSystem(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, []string{"On", "ForceOff", "GracefulRestart"})
	sys, err := f.client(t).System(context.Background())
	if err != nil {
		t.Fatalf("System failed: %v", err)
	}
	if got, want := sys.PowerState, "On"; got != want {
		t.Errorf("PowerState = %q, want %q", got, want)
	}
	if got, want := sys.Health, "OK"; got != want {
		t.Errorf("Health = %q, want %q", got, want)
	}
	if got, want := strings.Join(sys.ResetTypes, ","), "On,ForceOff,GracefulRestart"; got != want {
		t.Errorf("ResetTypes = %q, want %q", got, want)
	}
	if got, want := strings.Join(sys.BootTargets, ","), "None,Pxe,Hdd"; got != want {
		t.Errorf("BootTargets = %q, want %q", got, want)
	}
}

func TestResetChecksWhatTheMachineAccepts(t *testing.T) {
	t.Parallel()

	// This firmware has no GracefulShutdown, which is exactly the case that
	// made the shell tool send a reset type no BMC would take.
	f := newFakeBMC(t, []string{"On", "ForceOff", "ForceRestart"})
	c := f.client(t)

	err := c.Reset(context.Background(), redfish.ResetGracefulShutdown)
	if err == nil {
		t.Fatal("an unsupported reset type should be refused before it is sent")
	}
	if !strings.Contains(err.Error(), "it accepts") {
		t.Errorf("error = %v, want it to list what the machine accepts", err)
	}
	if got := f.resets.Load(); got != 0 {
		t.Errorf("the action was sent %d times although it was refused", got)
	}

	if err := c.Reset(context.Background(), redfish.ResetForceOff); err != nil {
		t.Fatalf("a supported reset type should go through: %v", err)
	}
	if got := f.resets.Load(); got != 1 {
		t.Errorf("the action was sent %d times, want exactly once", got)
	}
	if got, want := f.lastPost["ResetType"], "ForceOff"; got != want {
		t.Errorf("ResetType = %v, want %v", got, want)
	}
}

func TestResetIsNotRetried(t *testing.T) {
	t.Parallel()

	// A reset that timed out may already have been carried out, so nothing
	// may resend it: a repeat would power cycle a running machine.
	f := newFakeBMC(t, nil)
	c := f.client(t)
	if err := c.Reset(context.Background(), redfish.ResetForceRestart); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}
	if got := f.resets.Load(); got != 1 {
		t.Errorf("the action was sent %d times, want exactly once", got)
	}
}

func TestBootOverrideDefaultsToOnce(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, nil)
	c := f.client(t)

	if err := c.SetBootOverride(context.Background(), "Pxe", false); err != nil {
		t.Fatalf("SetBootOverride failed: %v", err)
	}
	if len(f.patches) != 1 {
		t.Fatalf("got %d patches, want 1", len(f.patches))
	}
	boot := f.patches[0]["Boot"].(map[string]any)
	if got, want := boot["BootSourceOverrideEnabled"], "Once"; got != want {
		t.Errorf("override = %v, want %v; a persistent override reinstalls in a loop", got, want)
	}

	if err := c.SetBootOverride(context.Background(), "Pxe", true); err != nil {
		t.Fatalf("SetBootOverride failed: %v", err)
	}
	boot = f.patches[1]["Boot"].(map[string]any)
	if got, want := boot["BootSourceOverrideEnabled"], "Continuous"; got != want {
		t.Errorf("override = %v, want %v", got, want)
	}

	if err := c.SetBootOverride(context.Background(), "Floppy", false); err == nil {
		t.Error("a boot source the machine does not offer should be refused")
	}
}

func TestClearBootOverride(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, nil)
	if err := f.client(t).ClearBootOverride(context.Background()); err != nil {
		t.Fatalf("ClearBootOverride failed: %v", err)
	}
	boot := f.patches[0]["Boot"].(map[string]any)
	if got, want := boot["BootSourceOverrideEnabled"], "Disabled"; got != want {
		t.Errorf("override = %v, want %v", got, want)
	}
}

func TestErrorsCarryTheRedfishMessage(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, nil)
	_, err := f.client(t).Get(context.Background(), "/redfish/v1/broken")
	if err == nil {
		t.Fatal("a 400 should be reported")
	}
	for _, want := range []string{"Unsupported value", "Use a supported value."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to carry %q", err, want)
		}
	}
}

func TestBadCredentialsAreReported(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, nil)
	c := f.client(t)
	c.Password = "wrong"
	if _, err := c.System(context.Background()); err == nil {
		t.Error("a rejected password should be reported")
	}
}

func TestPinStore(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pins")
	store := &redfish.PinStore{Path: path}
	ctx := context.Background()

	if _, ok, err := store.Get("bmc1"); err != nil || ok {
		t.Fatalf("an empty store returned %v, %v", ok, err)
	}
	if err := store.Set(ctx, "bmc1", "sha256:aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "bmc2", "sha256:bbbb"); err != nil {
		t.Fatal(err)
	}
	pin, ok, err := store.Get("bmc1")
	if err != nil || !ok || pin != "sha256:aaaa" {
		t.Fatalf("Get = %q, %v, %v", pin, ok, err)
	}
	if err := store.Remove(ctx, "bmc1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.Get("bmc1"); ok {
		t.Error("the pin was not removed")
	}
	if _, ok, _ := store.Get("bmc2"); !ok {
		t.Error("removing one pin dropped another")
	}
}

func TestPinMismatchExplainsTheWayOut(t *testing.T) {
	t.Parallel()

	err := &redfish.PinMismatchError{Host: "bmc1", Recorded: "sha256:aaaa", Seen: "sha256:bbbb"}
	msg := err.Error()
	for _, want := range []string{"bmc1", "sha256:aaaa", "sha256:bbbb", "bmc forget"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// Section 5.12 of the September 2026 review: a processor that cannot be
// reached exits 3, like any other host, and not 1 as if it had refused.
func TestUnreachableProcessorIsATransportFailure(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	c := &redfish.Client{Host: "example.com", Username: "admin", Password: "secret",
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}}
	_, err = c.PowerState(context.Background())
	if err == nil {
		t.Fatal("a closed port answered")
	}
	if got, want := exitcode.From(err), exitcode.Transport; got != want {
		t.Errorf("exit code = %d, want %d (%v)", got, want, err)
	}
}

// A processor that turns the account away could not be authenticated with,
// which exits 3 too; one that refuses a request exits 1.
func TestRejectedAccountIsATransportFailure(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, []string{"On"})
	c := f.client(t)
	c.Password = "wrong"
	_, err := c.PowerState(context.Background())
	if got, want := exitcode.From(err), exitcode.Transport; got != want {
		t.Errorf("exit code = %d, want %d (%v)", got, want, err)
	}

	err = f.client(t).SetBootOverride(context.Background(), "Floppy", false)
	if err == nil {
		t.Fatal("an unsupported boot source was accepted")
	}
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("a refusal: exit code = %d, want %d (%v)", got, want, err)
	}
}
