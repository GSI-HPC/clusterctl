// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// dhcpApp is newApp with a cached copy of dhcpd.conf that expires at once,
// so that nothing but the command's own memory keeps it from being fetched
// again.
func dhcpApp(t *testing.T, rec *transport.Recorder) *app.App {
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
		Set:         map[string]string{"services.dhcp.cacheTtl": "1ns"},
		Runner:      rec,
	})
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}
	return a
}

const dhcpdConf = "host exe0002 {\n  hardware ethernet aa:bb:cc:00:00:02;\n  fixed-address 10.0.2.2;\n}\n"

// Every node without an inventory address read the DHCP configuration
// again, so a server that could not be reached was asked once per node, an
// ssh timeout each, and a good one was fetched once per node once its cached
// copy had expired. One command reads it once, however many callers ask and
// however many of them ask at the same time, and a failure is kept like an
// answer.
func TestDHCPConfigIsReadOncePerCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		reply func(transport.Target) *transport.Result
		code  int
	}{
		{"a server that answers", func(tg transport.Target) *transport.Result {
			return &transport.Result{Target: tg, Stdout: dhcpdConf}
		}, exitcode.OK},
		{"a server that cannot be reached", func(tg transport.Target) *transport.Result {
			return transport.ExitResult(tg, 255, "", "ssh: connect to host "+tg.Host+" port 22: Connection timed out\n")
		}, exitcode.Transport},
		{"a file that is not there", func(tg transport.Target) *transport.Result {
			return transport.ExitResult(tg, 1, "", "cat: /etc/dhcp/dhcpd.conf: No such file or directory\n")
		}, exitcode.TargetFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reads atomic.Int32
			a := dhcpApp(t, &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
				reads.Add(1)
				// Long enough for every caller to arrive while it runs.
				time.Sleep(50 * time.Millisecond)
				return tc.reply(tg), nil
			}})

			errs := make([]error, 9)
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Go(func() { _, errs[i] = a.DHCPConfig(context.Background()) })
			}
			wg.Wait()
			cfg, err := a.DHCPConfig(context.Background())
			errs[8] = err

			if n := reads.Load(); n != 1 {
				t.Errorf("dhcpd.conf was read %d times, want once", n)
			}
			for i, err := range errs {
				if got := exitcode.From(err); got != tc.code {
					t.Errorf("caller %d: exit code = %d (%v), want %d", i, got, err, tc.code)
				}
			}
			if tc.code == exitcode.OK {
				if address, err := cfg.BootAddress("exe0002"); err != nil || address != "10.0.2.2" {
					t.Errorf("exe0002 boots with %q (%v), want 10.0.2.2", address, err)
				}
			}
		})
	}
}

// A read that an interrupt stopped says nothing about the server, so it is
// not what the next caller is told.
func TestDHCPConfigForgetsAnInterruptedRead(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	a := dhcpApp(t, &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if reads.Add(1) == 1 {
			return &transport.Result{Target: tg, ExitCode: -1,
				Err: exitcode.Wrap(exitcode.Interrupted, fmt.Errorf("%s: %w", tg, context.Canceled))}, nil
		}
		return &transport.Result{Target: tg, Stdout: dhcpdConf}, nil
	}})

	interrupted, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.DHCPConfig(interrupted); exitcode.From(err) != exitcode.Interrupted {
		t.Fatalf("the interrupted read: error %v, want an interrupt", err)
	}
	if _, err := a.DHCPConfig(context.Background()); err != nil {
		t.Errorf("the read after the interrupt: %v, want the configuration", err)
	}
	if n := reads.Load(); n != 2 {
		t.Errorf("dhcpd.conf was read %d times, want twice", n)
	}
}
