// SPDX-License-Identifier: LGPL-3.0-or-later

package app_test

import (
	"context"
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
