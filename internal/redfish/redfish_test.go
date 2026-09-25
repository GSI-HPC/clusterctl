// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package redfish_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// fakeBMC serves the part of the Redfish surface clusterctl uses.
type fakeBMC struct {
	server   *httptest.Server
	resets   atomic.Int32
	lastPost map[string]any
	patches  []map[string]any
	power    string
	// actionInfo serves the reset types through @Redfish.ActionInfo
	// instead of inline, as some firmware does.
	actionInfo atomic.Bool
	// actionInfoBroken makes the ActionInfo resource fail.
	actionInfoBroken atomic.Bool
	// mux serves the resources; tests add their own to it.
	mux *http.ServeMux
	// requests counts what reached the server, and withAuth how much of
	// it carried credentials.
	requests atomic.Int32
	withAuth atomic.Int32
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
			switch {
			case f.actionInfo.Load():
				allowable["@Redfish.ActionInfo"] = "/redfish/v1/Systems/1/ResetActionInfo"
			case resetTypes != nil:
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
	mux.HandleFunc("/redfish/v1/Systems/1/ResetActionInfo", func(w http.ResponseWriter, _ *http.Request) {
		if f.actionInfoBroken.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Parameters": []any{
				map[string]any{"Name": "Other", "AllowableValues": toAny([]string{"GracefulShutdown"})},
				map[string]any{"Name": "ResetType", "AllowableValues": toAny(resetTypes)},
			},
		})
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

	f.mux = mux
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			f.withAuth.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
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

// dialTo connects to one address whatever host a request names.
func dialTo(addr string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
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

// A refused request reads and exits as it did before it had a type of its
// own, and tells a progress display why it failed: a 401 or a 403 turned
// the account away, and any other refusal is the processor's own, which
// the exit code classes.
func TestARefusedRequestSaysWhyItWasRefused(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, nil)
	f.mux.HandleFunc("/redfish/v1/refuse/{status}", func(w http.ResponseWriter, r *http.Request) {
		status, _ := strconv.Atoi(r.PathValue("status"))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "go away"}})
	})
	for _, tc := range []struct {
		status int
		want   string
		code   int
		class  progress.Class
	}{
		{http.StatusUnauthorized, "example.com: 401 Unauthorized: go away", exitcode.Transport, progress.ClassAuth},
		{http.StatusForbidden, "example.com: 403 Forbidden: go away", exitcode.TargetFailed, progress.ClassAuth},
		{http.StatusBadRequest, "example.com: 400 Bad Request: go away", exitcode.TargetFailed, progress.ClassTarget},
		{http.StatusNotFound, "example.com: 404 Not Found: go away", exitcode.TargetFailed, progress.ClassTarget},
		{http.StatusInternalServerError, "example.com: 500 Internal Server Error: go away", exitcode.TargetFailed, progress.ClassTarget},
	} {
		_, err := f.client(t).Get(context.Background(), "/redfish/v1/refuse/"+strconv.Itoa(tc.status))
		if got := fmt.Sprint(err); got != tc.want {
			t.Errorf("%d: error = %q, want %q", tc.status, got, tc.want)
		}
		if got := exitcode.From(err); got != tc.code {
			t.Errorf("%d: exit code = %d, want %d", tc.status, got, tc.code)
		}
		if got := progress.Classify(err); got != tc.class {
			t.Errorf("%d: class = %s, want %s", tc.status, got, tc.class)
		}
		var refused *redfish.StatusError
		if !errors.As(err, &refused) || refused.StatusCode != tc.status {
			t.Errorf("%d: error = %#v, want a StatusError with that status", tc.status, err)
		}
	}

	// The account the fake turns away, as a wrong password is.
	c := f.client(t)
	c.Password = "wrong"
	_, err := c.PowerState(context.Background())
	if got, want := fmt.Sprint(err), "example.com: 401 Unauthorized: "; got != want {
		t.Errorf("a wrong password: error = %q, want %q", got, want)
	}
	if got := progress.Classify(err); got != progress.ClassAuth {
		t.Errorf("a wrong password: class = %s, want %s", got, progress.ClassAuth)
	}
}

func TestResetChecksTheVendorProfile(t *testing.T) {
	t.Parallel()

	// The machine advertises GracefulShutdown, but the site recorded that
	// this firmware rejects it, so the recorded list decides.
	f := newFakeBMC(t, []string{"On", "ForceOff", "GracefulShutdown"})
	c := f.client(t)
	c.ResetTypes = []string{"On", "ForceOff", "ForceRestart"}

	err := c.Reset(context.Background(), redfish.ResetGracefulShutdown)
	if err == nil {
		t.Fatal("a reset type the vendor profile leaves out should be refused before it is sent")
	}
	if !strings.Contains(err.Error(), "ForceOff, ForceRestart, On") {
		t.Errorf("error = %v, want it to list the vendor profile", err)
	}
	if got := f.resets.Load(); got != 0 {
		t.Errorf("the action was sent %d times although it was refused", got)
	}
	if err := c.Reset(context.Background(), redfish.ResetForceRestart); err != nil {
		t.Fatalf("a reset type the vendor profile lists should go through: %v", err)
	}
	if got := f.resets.Load(); got != 1 {
		t.Errorf("the action was sent %d times, want exactly once", got)
	}
}

func TestResetFollowsActionInfo(t *testing.T) {
	t.Parallel()

	// Some firmware lists the reset types only in a separate ActionInfo
	// resource. Without following it, GracefulShutdown went out unchecked
	// and the BMC's own opaque rejection came back.
	f := newFakeBMC(t, []string{"On", "ForceOff", "ForceRestart"})
	f.actionInfo.Store(true)
	c := f.client(t)

	err := c.Reset(context.Background(), redfish.ResetGracefulShutdown)
	if err == nil {
		t.Fatal("a reset type missing from ActionInfo should be refused before it is sent")
	}
	if !strings.Contains(err.Error(), "ForceOff, ForceRestart, On") {
		t.Errorf("error = %v, want it to list what ActionInfo offers", err)
	}
	if got := f.resets.Load(); got != 0 {
		t.Errorf("the action was sent %d times although it was refused", got)
	}

	sys, err := c.System(context.Background())
	if err != nil {
		t.Fatalf("System failed: %v", err)
	}
	if got, want := strings.Join(sys.ResetTypes, ","), "On,ForceOff,ForceRestart"; got != want {
		t.Errorf("ResetTypes = %q, want %q", got, want)
	}

	if err := c.Reset(context.Background(), redfish.ResetForceOff); err != nil {
		t.Fatalf("a reset type ActionInfo lists should go through: %v", err)
	}
	if got := f.resets.Load(); got != 1 {
		t.Errorf("the action was sent %d times, want exactly once", got)
	}
}

func TestResetRefusesWhenActionInfoCannotBeRead(t *testing.T) {
	t.Parallel()

	// The machine says where its list is, so a reset is not sent unchecked
	// when that list cannot be read.
	f := newFakeBMC(t, []string{"On", "ForceOff"})
	f.actionInfo.Store(true)
	f.actionInfoBroken.Store(true)

	if err := f.client(t).Reset(context.Background(), redfish.ResetForceOff); err == nil {
		t.Fatal("a reset should be refused when the ActionInfo it points to cannot be read")
	}
	if got := f.resets.Load(); got != 0 {
		t.Errorf("the action was sent %d times although its check failed", got)
	}
}

// credentialSink is a plain HTTP server that records every request it gets,
// standing in for wherever a redirect points.
type credentialSink struct {
	server *httptest.Server
	hits   atomic.Int32
	auth   atomic.Bool
}

func newCredentialSink(t *testing.T) *credentialSink {
	t.Helper()
	s := &credentialSink{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			s.auth.Store(true)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"PowerState": "On"})
	}))
	t.Cleanup(s.server.Close)
	return s
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
		post   bool
		// toSink points the redirect at plain HTTP on another port of the
		// same host, which Go's default policy sends the credentials to.
		toSink bool
	}{
		{name: "a read is not sent to plain HTTP with the credentials", status: http.StatusTemporaryRedirect, toSink: true},
		{name: "a reset is not sent again after a 307", status: http.StatusTemporaryRedirect, post: true},
		{name: "a reset is not sent again after a 308", status: http.StatusPermanentRedirect, post: true},
		{name: "a reset is not sent to plain HTTP", status: http.StatusPermanentRedirect, post: true, toSink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sink := newCredentialSink(t)
			_, sinkPort, _ := net.SplitHostPort(sink.server.Listener.Addr().String())
			f := newFakeBMC(t, nil)
			f.mux.HandleFunc("/redfish/v1/moved", func(w http.ResponseWriter, r *http.Request) {
				target := "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset"
				if r.Method == http.MethodGet {
					target = "/redfish/v1/Systems/1"
				}
				if tc.toSink {
					target = "http://example.com:" + sinkPort + target
				}
				http.Redirect(w, r, target, tc.status)
			})

			// The client names example.com throughout; its port decides
			// whether a connection reaches the sink or the fake BMC.
			c := f.client(t)
			rt := c.Transport.(*http.Transport)
			bmc, sinkAddr := f.server.Listener.Addr().String(), sink.server.Listener.Addr().String()
			rt.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				to := bmc
				if strings.HasSuffix(addr, ":"+sinkPort) {
					to = sinkAddr
				}
				var d net.Dialer
				return d.DialContext(ctx, network, to)
			}
			var err error
			if tc.post {
				_, err = c.Post(context.Background(), "/redfish/v1/moved", map[string]any{"ResetType": "ForceRestart"})
			} else {
				_, err = c.Get(context.Background(), "/redfish/v1/moved")
			}
			if err == nil {
				t.Error("a redirect should be reported, not followed")
			} else if !strings.Contains(err.Error(), "redirect") {
				t.Errorf("error = %v, want it to say the answer was a redirect", err)
			}
			if got := f.resets.Load(); got != 0 {
				t.Errorf("the reset was sent again %d times after a redirect", got)
			}
			if got := sink.hits.Load(); got != 0 {
				t.Errorf("the redirect target got %d requests", got)
			}
			if sink.auth.Load() {
				t.Error("the credentials were sent in cleartext")
			}
		})
	}
}

// pinningClient talks to the fake BMC through the real pinning transport,
// which the other tests bypass.
func (f *fakeBMC) pinningClient(t *testing.T, store *redfish.PinStore) *redfish.Client {
	t.Helper()
	c := f.client(t)
	c.Transport = nil
	c.DialContext = dialTo(f.server.Listener.Addr().String())
	c.Pins = store
	return c
}

func (f *fakeBMC) fingerprint() string {
	return redfish.Fingerprint(f.server.Certificate())
}

func TestPinningRecordsTheFirstCertificate(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, nil)
	store := &redfish.PinStore{Path: filepath.Join(t.TempDir(), "pins")}
	c := f.pinningClient(t, store)
	host := c.Host

	if _, err := c.System(context.Background()); err != nil {
		t.Fatalf("first contact failed: %v", err)
	}
	pin, ok, err := store.Get(host)
	if err != nil || !ok {
		t.Fatalf("no pin was recorded for %s: %v", host, err)
	}
	if want := f.fingerprint(); pin != want {
		t.Errorf("pin = %q, want the server's certificate %q", pin, want)
	}

	// A fresh client, as the next command would build, is checked against
	// the recorded pin and let through.
	if _, err := f.pinningClient(t, store).System(context.Background()); err != nil {
		t.Fatalf("a matching certificate was refused: %v", err)
	}
}

func TestPinningRefusesAChangedCertificate(t *testing.T) {
	t.Parallel()

	// Every httptest server presents the same certificate, so the pin of
	// an earlier certificate is written directly.
	const earlier = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	f := newFakeBMC(t, nil)
	store := &redfish.PinStore{Path: filepath.Join(t.TempDir(), "pins")}
	c := f.pinningClient(t, store)
	if err := store.Set(context.Background(), c.Host, earlier); err != nil {
		t.Fatal(err)
	}

	_, err := c.Post(context.Background(), "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
		map[string]any{"ResetType": "ForceOff"})
	var mismatch *redfish.PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want a PinMismatchError", err)
	}
	if mismatch.Recorded != earlier || mismatch.Seen != f.fingerprint() {
		t.Errorf("mismatch = %+v, want recorded %s and seen %s", mismatch, earlier, f.fingerprint())
	}
	if got, want := exitcode.From(err), exitcode.Transport; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if got, want := progress.Classify(err), progress.ClassPin; got != want {
		t.Errorf("class = %s, want %s", got, want)
	}
	if got := f.requests.Load(); got != 0 {
		t.Errorf("%d requests reached the server behind a changed certificate", got)
	}
	if got := f.withAuth.Load(); got != 0 {
		t.Errorf("the credentials were sent %d times to a changed certificate", got)
	}
	if got := f.resets.Load(); got != 0 {
		t.Errorf("the reset was sent %d times to a changed certificate", got)
	}
	if pin, _, _ := store.Get(c.Host); pin != earlier {
		t.Errorf("the recorded pin was replaced by %q", pin)
	}
}

func TestPinStoreDoesNotReplaceAPin(t *testing.T) {
	t.Parallel()

	store := &redfish.PinStore{Path: filepath.Join(t.TempDir(), "pins")}
	ctx := context.Background()

	if err := store.Set(ctx, "bmc1", "sha256:aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "bmc1", "sha256:aaaa"); err != nil {
		t.Errorf("recording the same pin again failed: %v", err)
	}
	err := store.Set(ctx, "bmc1", "sha256:bbbb")
	var mismatch *redfish.PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want a PinMismatchError", err)
	}
	if mismatch.Recorded != "sha256:aaaa" || mismatch.Seen != "sha256:bbbb" {
		t.Errorf("mismatch = %+v", mismatch)
	}
	if pin, _, _ := store.Get("bmc1"); pin != "sha256:aaaa" {
		t.Errorf("pin = %q, want the first one kept", pin)
	}
}

func TestPinStoreFirstContactsRace(t *testing.T) {
	t.Parallel()

	// Overlapping first contacts that present different certificates, as a
	// man in the middle could arrange: only one may be accepted.
	store := &redfish.PinStore{Path: filepath.Join(t.TempDir(), "pins")}
	const contacts = 8
	errs := make([]error, contacts)
	var wg sync.WaitGroup
	for i := range contacts {
		wg.Go(func() {
			errs[i] = store.Set(context.Background(), "bmc1", fmt.Sprintf("sha256:%04d", i))
		})
	}
	wg.Wait()

	pin, ok, err := store.Get("bmc1")
	if err != nil || !ok {
		t.Fatalf("no pin was recorded: %v", err)
	}
	accepted := 0
	for i, err := range errs {
		var mismatch *redfish.PinMismatchError
		switch {
		case err == nil:
			accepted++
			if want := fmt.Sprintf("sha256:%04d", i); pin != want {
				t.Errorf("contact %d was accepted, but %q is recorded", i, pin)
			}
		case errors.As(err, &mismatch):
		default:
			t.Errorf("contact %d: %v", i, err)
		}
	}
	if accepted != 1 {
		t.Errorf("%d different certificates were accepted at first contact, want 1", accepted)
	}
}

// A client built without a pin store accepted any certificate without a
// word and sent the account to whoever presented it. Every command sets a
// store, so only a direct user of the package was exposed; it is refused.
func TestAClientWithoutAPinStoreIsRefused(t *testing.T) {
	t.Parallel()

	for name, store := range map[string]*redfish.PinStore{
		"no store":               nil,
		"a store without a path": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeBMC(t, nil)
			_, err := f.pinningClient(t, store).System(context.Background())
			if err == nil {
				t.Fatal("a client that pins nothing reached the service processor")
			}
			if !strings.Contains(err.Error(), "pin") {
				t.Errorf("error = %v, want it to say that no pin store is set", err)
			}
			if got := f.requests.Load(); got != 0 {
				t.Errorf("the service processor got %d requests, want none", got)
			}
		})
	}
}

func TestAClientDoesNotPrintItsPassword(t *testing.T) {
	t.Parallel()

	c := redfish.Client{Host: "bmc1", Username: "admin", Password: "hunter2"}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{c, &c} {
			if got := fmt.Sprintf(verb, v); strings.Contains(got, "hunter2") || !strings.Contains(got, "bmc1") {
				t.Errorf("Sprintf(%q) = %q, want the host and no password", verb, got)
			}
		}
	}
}

// openConns counts the connections a server holds open.
type openConns struct {
	mu   sync.Mutex
	open int
}

func (o *openConns) track(_ net.Conn, state http.ConnState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch state {
	case http.StateNew:
		o.open++
	case http.StateClosed, http.StateHijacked:
		o.open--
	}
}

func (o *openConns) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.open
}

// A client kept the connection of its last request open with nothing to
// close it, and every client has its own: the MCP server, which runs for
// days, held one to each processor it had talked to for as long as the
// processor let it, and a processor has few. A client lets go of it when
// told to, and of one it has left idle for as long as a request may take.
func TestAClientLetsGoOfItsConnection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		timeout time.Duration
		done    func(*redfish.Client)
	}{
		{"when it is done with the processor", time.Minute, (*redfish.Client).CloseIdleConnections},
		{"when the connection has been idle for as long as a request may take", 500 * time.Millisecond, func(*redfish.Client) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conns := &openConns{}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"PowerState":"On"}`))
			}))
			server.Config.ConnState = conns.track
			server.StartTLS()
			t.Cleanup(server.Close)
			c := &redfish.Client{
				Host:        "example.com",
				Username:    "admin",
				Password:    "secret",
				Timeout:     tc.timeout,
				Pins:        &redfish.PinStore{Path: filepath.Join(t.TempDir(), "pins")},
				DialContext: dialTo(server.Listener.Addr().String()),
			}

			if _, err := c.PowerState(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := conns.count(); got != 1 {
				t.Fatalf("%d connections are open after the request, want the one it used", got)
			}
			tc.done(c)
			deadline := time.Now().Add(5 * time.Second)
			for conns.count() > 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := conns.count(); got != 0 {
				t.Errorf("%d connections are still open, want none", got)
			}
		})
	}
}
