// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

const exampleDir = "../../examples/site"

func newApp(t *testing.T, rec *transport.Recorder) *app.App {
	t.Helper()
	dir := t.TempDir()
	a, err := app.New(context.Background(), app.Streams{
		In:       strings.NewReader(""),
		Out:      &strings.Builder{},
		Err:      &strings.Builder{},
		StateDir: filepath.Join(dir, "state"),
		CacheDir: filepath.Join(dir, "cache"),
	}, app.Options{
		ConfigFiles: []string{exampleDir},
		Env:         func(string) string { return "" },
		Runner:      rec,
	})
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}
	return a
}

func TestNewResolvesEverything(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	if got, want := a.Resolved.Context.Name, "cluster1"; got != want {
		t.Errorf("context = %q, want %q", got, want)
	}
	if a.Inventory.Len() == 0 {
		t.Error("the inventory is empty")
	}
	if a.Namer == nil || a.Groups == nil || a.Gate == nil || a.SSH == nil {
		t.Error("the command context is incomplete")
	}
}

func TestNewReportsAMissingConfiguration(t *testing.T) {
	t.Parallel()

	_, err := app.New(context.Background(), app.Streams{StateDir: t.TempDir()}, app.Options{
		ConfigFiles: []string{filepath.Join(t.TempDir(), "nothing")},
		Env:         func(string) string { return "" },
	})
	if err == nil {
		t.Fatal("a missing configuration should be reported")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
}

func TestRole(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	target, err := a.Role("mgmt")
	if err != nil {
		t.Fatalf("Role failed: %v", err)
	}
	if got, want := target.Host, "mgmt-gw.example.org"; got != want {
		t.Errorf("host = %q, want %q", got, want)
	}
	if !target.ForwardAgent {
		t.Error("the role asks for agent forwarding and did not get it")
	}

	_, err = a.Role("nope")
	if err == nil {
		t.Fatal("an unknown role should be reported")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Errorf("error = %v, want it to list the configured roles", err)
	}
}

func TestNodeAppliesTheNamingRules(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	target, err := a.Node("exe0001")
	if err != nil {
		t.Fatalf("Node failed: %v", err)
	}
	if got, want := target.Host, "exe0001.hpc.example.org"; got != want {
		t.Errorf("host = %q, want %q", got, want)
	}
}

func TestSelectResolvesGroupsAndCanonicalisesNames(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	ns, err := a.Select("@inventory:exe!exe0003")
	if err != nil {
		t.Fatalf("Select failed: %v", err)
	}
	if got, want := ns.String(), "exe[0001-0002,0004-0010]"; got != want {
		t.Errorf("Select = %q, want %q", got, want)
	}

	// The inventory wrote exe0001, so a short name comes back under the
	// name the site gave the machine.
	ns, err = a.Select("exe[1-2]")
	if err != nil {
		t.Fatalf("Select failed: %v", err)
	}
	if got, want := ns.String(), "exe[0001-0002]"; got != want {
		t.Errorf("Select = %q, want %q", got, want)
	}

	if _, err := a.Select(""); err == nil {
		t.Error("selecting nothing should be reported")
	}
	if _, err := a.Select("exe["); err == nil {
		t.Error("a malformed expression should be reported")
	}
}

func TestSelectOptional(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	ns, err := a.SelectOptional("")
	if err != nil || ns != nil {
		t.Errorf("SelectOptional(\"\") = %v, %v, want nil, nil", ns, err)
	}
}

func TestRemoteFileIsCached(t *testing.T) {
	t.Parallel()

	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: "content\n"}, nil
	}}
	a := newApp(t, rec)

	for i := 0; i < 3; i++ {
		data, err := a.RemoteFile(context.Background(), "dhcp", "/etc/dhcp/dhcpd.conf", time.Minute)
		if err != nil {
			t.Fatalf("RemoteFile failed: %v", err)
		}
		if string(data) != "content\n" {
			t.Errorf("content = %q", data)
		}
	}
	// Fetching once per node turns a node set query into one connection per
	// node, which is what the cache is for.
	if got := len(rec.Calls()); got != 1 {
		t.Errorf("the file was fetched %d times, want once", got)
	}
}

func TestRemoteFileReportsAFailure(t *testing.T) {
	t.Parallel()

	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, ExitCode: 1, Stderr: "no such file\n"}, nil
	}}
	a := newApp(t, rec)

	if _, err := a.RemoteFile(context.Background(), "dhcp", "/missing", 0); err == nil {
		t.Error("a failing read should be reported")
	}
}

func TestBMCOrderPrefersRedfish(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	// IPMI over LAN ships disabled on current firmware, so trying it first
	// only produces timeouts.
	if got, want := a.PreferredBMCTransport("exe0001"), "redfish"; got != want {
		t.Errorf("preferred transport = %q, want %q", got, want)
	}
	profile := a.VendorProfile("exe0001")
	if len(profile.ResetTypes) == 0 {
		t.Error("the vendor profile of exe0001 was not found")
	}
}

// Any first entry other than ipmi selected Redfish, so a misspelt impi sent
// the BMC account over the transport the site had ruled out.
func TestBMCTransportsRefusesAnUnknownEntry(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	a.Spec.BMC.Order = []string{"IPMI", "redfish", "ipmi"}
	got, err := a.BMCTransports("exe0001")
	if err != nil || strings.Join(got, ",") != "ipmi,redfish" {
		t.Errorf("BMCTransports = %v, %v; want ipmi,redfish", got, err)
	}

	a.Spec.BMC.Order = []string{"impi"}
	if _, err := a.BMCTransports("exe0001"); exitcode.From(err) != exitcode.Usage {
		t.Errorf("an unknown transport: err = %v, want a usage error", err)
	}
}

func TestPathResolvesAgainstTheSiteDocument(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	got := a.Path("ssh-known-hosts")
	if !strings.HasSuffix(got, filepath.Join("examples", "site", "ssh-known-hosts")) {
		t.Errorf("Path = %q, want it resolved against the site document", got)
	}
	if a.Path("") != "" {
		t.Error("an empty path should stay empty")
	}
}

// Review 9.11: a relative path given in the environment or with --set is
// resolved against the working directory, where it was typed, and one
// written in a document against the directory of the Site document.
func TestCommandLinePathsResolveAgainstTheWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  map[string]string
		set  map[string]string
		want string
	}{
		{"document", nil, nil, filepath.Join(exampleDir, "ssh-known-hosts")},
		{"environment", map[string]string{"CLUSTERCTL_KNOWN_HOSTS": "known"}, nil, filepath.Join(wd, "known")},
		{"--set", nil, map[string]string{"ssh.knownHostsFile": "known"}, filepath.Join(wd, "known")},
		{"--set section", nil, map[string]string{"ssh": "{knownHostsFile: known}"}, filepath.Join(wd, "known")},
		{"absolute", nil, map[string]string{"ssh.knownHostsFile": "/etc/known"}, "/etc/known"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := app.New(context.Background(), app.Streams{StateDir: t.TempDir(), CacheDir: t.TempDir()}, app.Options{
				ConfigFiles: []string{exampleDir},
				Env:         func(k string) string { return tc.env[k] },
				Set:         tc.set,
				Runner:      &transport.Recorder{},
			})
			if err != nil {
				t.Fatalf("building the app: %v", err)
			}
			if got := a.Path(a.Spec.SSH.KnownHostsFile); got != tc.want {
				t.Errorf("known hosts file = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedfishClientCarriesTheVendorResetTypes(t *testing.T) {
	t.Parallel()
	a, _ := bmcApp(t)

	// The example's vendor2 records the reset types its firmware accepts,
	// and the client has to check against them rather than ignore them.
	c, err := a.RedfishClient(context.Background(), "exe0001")
	if err != nil {
		t.Fatalf("RedfishClient failed: %v", err)
	}
	if got, want := strings.Join(c.ResetTypes, ","), strings.Join(a.VendorProfile("exe0001").ResetTypes, ","); got != want || got == "" {
		t.Errorf("client reset types = %q, want the vendor profile's %q", got, want)
	}

	c, err = a.RedfishClient(context.Background(), "wlm01")
	if err != nil {
		t.Fatalf("RedfishClient failed: %v", err)
	}
	if len(c.ResetTypes) != 0 {
		t.Errorf("a node without a vendor list got reset types %v", c.ResetTypes)
	}
}
