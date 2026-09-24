// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// helperEnv makes the test binary run one clusterctl command the way main
// does: under a context the first SIGINT or SIGTERM cancels, with the exit
// code report gives. Its value says which BMC stands behind the command.
const helperEnv = "CLUSTERCTL_BMC_HELPER"

// TestBMCInterruptHelperProcess is the process under test, not a test.
func TestBMCInterruptHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(helperEnv+"_ARGS")), &args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(99)
	}
	started := os.Getenv(helperEnv + "_STARTED")
	if mode == "redfish" {
		// A processor that takes its time: the first request says it
		// arrived, and every request waits until it is given up.
		redfishClientFor = func(a *app.App, ctx context.Context, node string) (*redfish.Client, error) {
			c, err := a.RedfishClient(ctx, node)
			if err != nil {
				return nil, err
			}
			c.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
				if f, err := os.OpenFile(started, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					_ = f.Close()
				}
				<-req.Context().Done()
				return nil, context.Cause(req.Context())
			})
			return c, nil
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	streams := app.DefaultStreams()
	cmd := NewRootCommand(ctx, streams)
	cmd.SetArgs(args)
	code := exitcode.OK
	if err := cmd.ExecuteContext(ctx); err != nil {
		code = report(streams, err)
	}
	os.Exit(code)
}

// slowSSH writes an ssh that notes that it started and then hangs, the way
// the IPMI backend does while it waits for unreachable processors.
func slowSSH(t *testing.T, started string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "ssh")
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = -V ] && { echo OpenSSH_9.6p1 >&2; exit 0; }\n: > '%s'\nexec sleep 60\n", started)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

// An interrupt while the requests were under way was reported as every
// processor failed, with "interrupt signal received" in each row, and the
// command exited 1. It has to exit 130, and say what was interrupted rather
// than what failed.
func TestBMCInterruptInFlightExits130(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process for each command")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}
	tests := []struct {
		name  string
		mode  string
		args  []string
		state string
	}{
		{"power off over IPMI", "ipmi", []string{"bmc", "power", "off", "--ipmi", "-y", "-n", "exe[0001-0003]"}, "outcome unknown"},
		{"power cycle over Redfish", "redfish", []string{"bmc", "power", "cycle", "--batch", "3", "-y", "-n", "exe[0001-0003]"}, "outcome unknown"},
		{"boot set over Redfish", "redfish", []string{"bmc", "boot", "set", "Pxe", "-y", "-n", "exe[0001-0003]"}, "outcome unknown"},
		{"status over Redfish", "redfish", []string{"bmc", "status", "--set", "bmc.order=[redfish]", "-n", "exe[0001-0003]"}, "interrupted"},
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		for _, tc := range tests {
			t.Run(sig.String()+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				started := filepath.Join(dir, "started")
				args := []string{"--config", exampleDir, "--set", "safety.slurmAware=false",
					"--set", "ssh.binary=" + slowSSH(t, started)}
				encoded, err := json.Marshal(append(args, tc.args...))
				if err != nil {
					t.Fatal(err)
				}

				cmd := exec.Command(os.Args[0], "-test.run=^TestBMCInterruptHelperProcess$")
				cmd.Env = append(os.Environ(),
					helperEnv+"="+tc.mode,
					helperEnv+"_ARGS="+string(encoded),
					helperEnv+"_STARTED="+started,
					"HOME="+dir,
					"XDG_STATE_HOME="+filepath.Join(dir, "state"),
					"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
					"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
					"CLUSTERCTL_CONFIG=", "CLUSTERCTL_NODES=", "CLUSTERCTL_CONTEXT=",
					"BMC_PASSWORD=s3cret",
				)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()

				// Wait until a request is under way, then interrupt.
				deadline := time.Now().Add(15 * time.Second)
				for {
					if _, err := os.Stat(started); err == nil {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("the command ended (%v) before a request was under way:\n%s%s", err, &stdout, &stderr)
					default:
					}
					if time.Now().After(deadline) {
						_ = cmd.Process.Kill()
						t.Fatalf("no request was ever sent:\n%s%s", &stdout, &stderr)
					}
					time.Sleep(20 * time.Millisecond)
				}
				_ = cmd.Process.Signal(sig)

				select {
				case <-done:
				case <-time.After(15 * time.Second):
					_ = cmd.Process.Kill()
					<-done
					t.Fatalf("still running 15s after the interrupt:\n%s%s", &stdout, &stderr)
				}
				if code := cmd.ProcessState.ExitCode(); code != exitcode.Interrupted {
					t.Errorf("exit code = %d, want %d:\n%s%s", code, exitcode.Interrupted, &stdout, &stderr)
				}
				if strings.Contains(stderr.String(), "failed") {
					t.Errorf("the interrupt was reported as a failure:\n%s", &stderr)
				}
				if !strings.Contains(stdout.String(), tc.state) {
					t.Errorf("no row reads %q:\n%s", tc.state, &stdout)
				}
			})
		}
	}
}
