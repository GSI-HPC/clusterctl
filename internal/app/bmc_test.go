// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

// bmcApp builds the example site with exe0003's service processor recorded
// in the inventory, and a BMC password in the environment.
func bmcApp(t *testing.T) (*app.App, *strings.Builder) {
	t.Helper()
	inventory, err := os.ReadFile(filepath.Join(exampleDir, "inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// A copy of the example with the inventory changed: a second document
	// of the same kind and name would be refused.
	extra := configtest.CopyDir(t, exampleDir, "inventory.yaml")
	doc := string(inventory) + "    - nodes: exe0003\n      bmcAddress: 10.9.0.77\n"
	if err := os.WriteFile(filepath.Join(extra, "inventory.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	errOut := &strings.Builder{}
	a, err := app.New(context.Background(), app.Streams{
		In:       strings.NewReader(""),
		Out:      &strings.Builder{},
		Err:      &lockedWriter{w: errOut},
		StateDir: filepath.Join(dir, "state"),
		CacheDir: filepath.Join(dir, "cache"),
	}, app.Options{
		ConfigFiles: []string{extra},
		Env: func(k string) string {
			if k == "BMC_PASSWORD" {
				return "s3cret"
			}
			return ""
		},
		Runner: &transport.Recorder{},
		// The example keeps its pins in the home directory.
		Set: map[string]string{"bmc.redfish.pinStore": filepath.Join(dir, "pins")},
	})
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}
	return a, errOut
}

type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// The inventory's bmcAddress was stored and shown, and never used: every
// command went to the name the rules derive, which a stale DNS record can
// point at another machine.
func TestBMCHostPrefersTheInventoryAddress(t *testing.T) {
	t.Parallel()
	a, _ := bmcApp(t)

	for node, want := range map[string]string{
		"exe0003": "10.9.0.77",
		"exe3":    "10.9.0.77",
		"exe0001": "exe0001.mgmt.hpc.example.org",
		// Not in the inventory: the rules still name it.
		"exe0042": "exe0042.mgmt.hpc.example.org",
	} {
		if got, err := a.BMCHost(node); err != nil || got != want {
			t.Errorf("BMCHost(%q) = %q, %v; want %q", node, got, err, want)
		}
	}

	bmcs, err := a.BMCHosts(nodeset.MustParse("exe[0002-0004]"))
	if err != nil {
		t.Fatalf("BMCHosts failed: %v", err)
	}
	for _, want := range []string{"10.9.0.77", "exe0002.mgmt.hpc.example.org", "exe0004.mgmt.hpc.example.org"} {
		if !bmcs.Contains(want) {
			t.Errorf("BMCHosts = %s, want it to contain %s", bmcs, want)
		}
	}

	// The certificate is pinned under the address the client talks to.
	client, err := a.RedfishClient(context.Background(), "exe0003")
	if err != nil {
		t.Fatalf("RedfishClient failed: %v", err)
	}
	if got, want := client.Host, "10.9.0.77"; got != want {
		t.Errorf("Redfish host = %q, want %q", got, want)
	}
}

// Every spelling of a machine gets its vendor profile, and with it the
// vendor's credential, transport order and Redfish settings. Only the typed
// name was looked up, so exe0001.hpc.example.org lost them while the command
// still reached exe0001's service processor.
func TestVendorProfileFollowsTheMachine(t *testing.T) {
	t.Parallel()
	a, _ := bmcApp(t)

	for _, spelling := range []string{"exe0001", "exe1", "EXE0001", "exe0001.", "exe0001.hpc.example.org", "Exe0001.HPC.example.org."} {
		if profile := a.VendorProfile(spelling); len(profile.ResetTypes) == 0 {
			t.Errorf("VendorProfile(%q) lost the vendor2 profile", spelling)
		}
		if got, err := a.BMCHost(spelling); err != nil || got != "exe0001.mgmt.hpc.example.org" {
			t.Errorf("BMCHost(%q) = %q, %v", spelling, got, err)
		}
	}
	// Another domain is another machine, which has no profile here.
	if profile := a.VendorProfile("exe0001.other.org"); len(profile.ResetTypes) != 0 {
		t.Error("exe0001.other.org was given exe0001's profile")
	}
}

// A node whose service processor cannot be named is refused, before a
// credential is read and before anything is contacted.
func TestBMCHostRefusesWhatItCannotName(t *testing.T) {
	t.Parallel()
	a, _ := bmcApp(t)

	for _, node := range []string{"10.0.2.1", "login01.hpc.example.org", "exe0001.other.org"} {
		if got, err := a.BMCHost(node); err == nil {
			t.Errorf("BMCHost(%q) = %q, want it refused", node, got)
		}
		_, err := a.RedfishClient(context.Background(), node)
		if err == nil {
			t.Errorf("RedfishClient(%q) should be refused", node)
		} else if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("RedfishClient(%q): exit code = %d, want %d", node, got, exitcode.Usage)
		}
	}
}

// The first certificate a service processor presents is recorded without
// anyone having vouched for it, so the administrator is told that it is.
func TestFirstContactIsReported(t *testing.T) {
	t.Parallel()
	a, errOut := bmcApp(t)

	if _, err := a.RedfishClient(context.Background(), "exe0003"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "10.9.0.77") || !strings.Contains(errOut.String(), "record") {
		t.Errorf("the first contact is not reported:\n%s", errOut)
	}
}

// The BMC commands build their clients in parallel. The credential resolver
// was created lazily without a lock, which the race detector reported.
func TestRedfishClientsAreBuiltConcurrently(t *testing.T) {
	t.Parallel()
	a, _ := bmcApp(t)

	var wg sync.WaitGroup
	for _, node := range nodeset.MustParse("exe[0001-0008]").Expand() {
		wg.Go(func() {
			client, err := a.RedfishClient(context.Background(), node)
			if err != nil || client.Password != "s3cret" {
				t.Errorf("RedfishClient(%q) = %v, %v", node, client, err)
			}
		})
	}
	wg.Wait()
}

// A password helper wrote its standard error to the process's own, which
// under clusterctl mcp only happened to be the server's log. It goes to the
// front end's diagnostics now, which are the command's error stream unless
// the front end, as the MCP server does, gives other ones: there the error
// stream is the notes a call returns to the agent.
func TestAPasswordHelperSpeaksToTheDiagnostics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		ownDiag bool
	}{
		{"on the error stream when the front end gives no diagnostics", false},
		{"in the diagnostics alone when the front end gives them", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			errOut, diag := &strings.Builder{}, &strings.Builder{}
			streams := app.Streams{
				In:       strings.NewReader(""),
				Out:      &strings.Builder{},
				Err:      errOut,
				StateDir: filepath.Join(dir, "state"),
				CacheDir: filepath.Join(dir, "cache"),
			}
			if tc.ownDiag {
				streams.Diag = diag
			}
			a, err := app.New(context.Background(), streams, app.Options{
				ConfigFiles: []string{exampleDir},
				Env:         func(string) string { return "" },
				Runner:      &transport.Recorder{},
				Set: map[string]string{
					"credentials.helper.username":         "admin",
					"credentials.helper.password.command": `[sh, -c, "echo unlocking the vault >&2; echo s3cret"]`,
				},
			})
			if err != nil {
				t.Fatalf("building the app: %v", err)
			}
			cred, err := a.Credentials().Get(context.Background(), "helper")
			if err != nil || cred.Password() != "s3cret" {
				t.Fatalf("Get = %v, %v", cred, err)
			}
			const said = "unlocking the vault"
			if got, want := strings.Contains(errOut.String(), said), !tc.ownDiag; got != want {
				t.Errorf("the helper's message is on the error stream = %t, want %t:\n%s", got, want, errOut)
			}
			if got, want := strings.Contains(diag.String(), said), tc.ownDiag; got != want {
				t.Errorf("the helper's message is in the diagnostics = %t, want %t:\n%s", got, want, diag)
			}
		})
	}
}

// The password prompt put its question on the terminal and started a read
// of it even when the command had already been interrupted, and the read
// could not be stopped: it could turn echo off after the terminal had been
// put back. Nothing is asked once the context has ended, and a lookup that
// read nothing for that reason is an interrupt rather than a problem with
// the configuration.
func TestThePromptAsksNothingOnceInterrupted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	errOut := &strings.Builder{}
	a, err := app.New(ctx, app.Streams{
		In:       strings.NewReader(""),
		Out:      &strings.Builder{},
		Err:      errOut,
		IsTTY:    true,
		StateDir: filepath.Join(dir, "state"),
		CacheDir: filepath.Join(dir, "cache"),
	}, app.Options{
		ConfigFiles: []string{exampleDir},
		Env:         func(string) string { return "" },
		Runner:      &transport.Recorder{},
	})
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}
	_, err = a.Credentials().Prompt("Password for admin@pdu: ")
	if !errors.Is(err, context.Canceled) || exitcode.From(err) != exitcode.Interrupted {
		t.Errorf("error = %v (exit code %d), want an interrupt", err, exitcode.From(err))
	}
	if errOut.Len() > 0 {
		t.Errorf("the prompt was shown after the interrupt:\n%s", errOut)
	}
	_, err = a.BMCCredential(ctx, "exe0001")
	if !errors.Is(err, context.Canceled) || exitcode.From(err) != exitcode.Interrupted {
		t.Errorf("BMCCredential = %v (exit code %d), want an interrupt", err, exitcode.From(err))
	}
}
