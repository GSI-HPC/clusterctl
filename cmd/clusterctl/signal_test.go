// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// childEnv makes the test binary act as the process under test: "main" runs
// clusterctl with the arguments it was given, and "wait" only waits for
// interrupts, which is how a read that ignores the context behaves.
const childEnv = "CLUSTERCTL_TEST_PROCESS"

func TestMain(m *testing.M) {
	switch os.Getenv(childEnv) {
	case "main":
		main()
	case "wait":
		ctx, _ := interruptContext()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("cancelled:", ctx.Err(), "cause:", context.Cause(ctx))
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// child starts the test binary as the process under test.
func child(t *testing.T, mode string, args ...string) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(),
		childEnv+"="+mode,
		"HOME="+dir,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"CLUSTERCTL_CONFIG=",
		"CLUSTERCTL_NODES=",
		"CLUSTERCTL_CONTEXT=",
	)
	return cmd
}

// waitFor waits for a process to end, killing it and failing the test if it
// is still running after d.
func waitFor(t *testing.T, cmd *exec.Cmd, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("the process was still running %v after it was interrupted", d)
		return nil
	}
}

// TestSecondInterruptEndsTheProcess checks that the first interrupt cancels
// the context and the second ends the process. The handler was never
// removed, so every later signal was swallowed and a read that ignored the
// context could not be interrupted at all.
func TestSecondInterruptEndsTheProcess(t *testing.T) {
	cmd := child(t, "wait")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	expect := func(prefix string) string {
		t.Helper()
		if !lines.Scan() || !strings.HasPrefix(lines.Text(), prefix) {
			_ = cmd.Process.Kill()
			t.Fatalf("the process said %q, want %q", lines.Text(), prefix)
		}
		return lines.Text()
	}

	expect("ready")
	_ = cmd.Process.Signal(os.Interrupt)
	if got := expect("cancelled"); got != "cancelled: context canceled cause: context canceled" {
		t.Errorf("the first interrupt gave %q; the cause has to be context.Canceled", got)
	}
	_ = cmd.Process.Signal(os.Interrupt)

	err = waitFor(t, cmd, 5*time.Second)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("the process ended with %v, want it killed by the signal", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Errorf("the process ended with %v, want it killed by SIGINT", exitErr)
	}
}

// fakeSSH writes an ssh that records that it started and then hangs, the way
// a remote command does that is still running when the administrator presses
// Ctrl-C.
func fakeSSH(t *testing.T) (binary, log string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}
	dir := t.TempDir()
	binary = filepath.Join(dir, "ssh")
	log = filepath.Join(dir, "started")
	// ssh -V is asked for the version before the configuration is written,
	// and is answered at once.
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = -V ] && { echo OpenSSH_9.6p1 >&2; exit 0; }\necho started >> '%s'\nexec sleep 60\n", log)
	writeScript(t, binary, script)
	return binary, log
}

// writeScript writes an executable script from a child process. Written
// from this one, the file would be open for writing while a parallel test
// forks, the child would inherit that descriptor until it execs, and running
// the script meanwhile fails with "text file busy".
func writeScript(t *testing.T, path, content string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `cat > "$1" && chmod 700 "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writing %s: %v: %s", path, err, out)
	}
}

// waitForFile waits until a file exists.
func waitForFile(t *testing.T, path string, cmd *exec.Cmd, stderr *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Fatalf("ssh was never started; stderr:\n%s", stderr)
}

// TestInterruptExits130 checks that the commands on main exit 130 when an
// interrupt stops a request in flight. A killed ssh was reported as
// "command exited -1", so slurm node drain exited 1 and the commands that
// run on a role exited 3, the codes for a failed and an unreachable host.
func TestInterruptExits130(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the process for each command")
	}
	commands := [][]string{
		{"slurm", "node", "drain", "ticket 1", "-n", "exe[0001-0003]", "-y"},
		{"boot", "status", "-n", "exe0001"},
		{"fabric", "counters", "exe0001"},
		{"exec", "-n", "exe[0001-0003]", "--", "sleep", "30"},
	}
	for _, signal := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		for _, args := range commands {
			t.Run(signal.String()+" "+strings.Join(args, " "), func(t *testing.T) {
				t.Parallel()
				binary, started := fakeSSH(t)
				cmd := child(t, "main", append([]string{
					"--config", "../../examples/site", "--set", "ssh.binary=" + binary,
				}, args...)...)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				waitForFile(t, started, cmd, &stderr)
				_ = cmd.Process.Signal(signal)

				err := waitFor(t, cmd, 15*time.Second)
				if code := cmd.ProcessState.ExitCode(); code != 130 {
					t.Errorf("exit code = %d (%v), want 130; stderr:\n%s", code, err, &stderr)
				}
				if strings.Contains(stderr.String(), "command exited") {
					t.Errorf("the interrupt was reported as a failed command:\n%s", &stderr)
				}
			})
		}
	}
}
