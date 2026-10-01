// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// A panic in the work for one node ended the whole process, because recover
// only catches a panic in its own goroutine and the fan-out workers had
// none. It is that node's failure now: the others are reported, and the
// command does not exit 0. The stack goes to the command's diagnostics,
// which are its standard error unless the front end says otherwise.
func TestAPanicOnOneNodeFailsOnlyThatNode(t *testing.T) {
	t.Parallel()
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if tg.Name == "exe0002" {
			panic("index out of range [3] with length 3")
		}
		return &transport.Result{Target: tg, Stdout: "Dell|R650|Dell|0A1B|Dell|2.1|2026-01-01|MT4123\n"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "-o", "json", "node", "hw", "-n", "exe[0001-0003]")
	wantCode(t, err, exitcode.TargetFailed)
	var rows []map[string]string
	if err := json.Unmarshal(h.out.Bytes(), &rows); err != nil {
		t.Fatalf("node hw printed no JSON: %v\n%s", err, h.out)
	}
	status := map[string]string{}
	for _, row := range rows {
		status[row["node"]] = row["status"]
	}
	for node, want := range map[string]string{"exe0001": "ok", "exe0002": "failed", "exe0003": "ok"} {
		if status[node] != want {
			t.Errorf("%s: status %q, want %q; rows: %v", node, status[node], want, rows)
		}
	}
	if log := h.errOut.String(); !strings.Contains(log, "panic while working on exe0002") || !strings.Contains(log, "goroutine") {
		t.Errorf("the stack of the panic was not written to standard error:\n%s", log)
	}
}

// The host key scan runs its own workers, which had no recover either.
func TestAPanicInAHostKeyScanFailsOnlyThatHost(t *testing.T) {
	host := startFakeHost(t)
	scanDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "exe0002.") {
			panic("index out of range [3] with length 3")
		}
		return (&net.Dialer{}).DialContext(ctx, network, host.address)
	}
	t.Cleanup(func() { scanDial = nil })

	h, err := run(t, harnessOptions{}, "hostkey", "scan", "-n", "exe[1-3]", "--timeout", "5s")
	if exitcode.From(err) == exitcode.OK {
		t.Fatalf("the scan succeeded although exe0002 panicked:\n%s", h.out)
	}
	if got := strings.Count(h.out.String(), "ssh-ed25519"); got != 2 {
		t.Errorf("%d keys collected, want those of the two other hosts:\n%s", got, h.out)
	}
	if !strings.Contains(h.out.String(), "panicked") {
		t.Errorf("the output does not say exe0002 panicked:\n%s", h.out)
	}
	if !strings.Contains(h.errOut.String(), "panic while working on exe0002.") {
		t.Errorf("the stack of the panic was not written to standard error:\n%s", h.errOut)
	}
}

// A panic while the secrets are written to a node is that node's failure:
// every file it did not report on fails with it, since the node was there
// to answer, and the other node is written. The push exits 1, and the
// stack goes to standard error.
func TestAPanicWritingTheSecretsOfANodeFailsOnlyThatNode(t *testing.T) {
	t.Parallel()
	dir, _ := secretSite{values: bmcSecret, identities: true, secrets: twoSecretFiles}.write(t)
	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if tg.Name == "exe0002" {
			panic("index out of range [3] with length 3")
		}
		return allWritten(tg, req)
	}}
	h, err := run(t, harnessOptions{config: []string{dir}, recorder: rec}, "secrets", "push", "-n", "exe[1-2]", "-y")
	wantCode(t, err, exitcode.TargetFailed)
	for _, want := range []string{
		"exe0002  /etc/munge/munge.key  failed: ",
		"exe0002  /etc/bmc.pass         failed: ",
		"exe0001  /etc/bmc.pass         written",
	} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output:\n%s\nwant a row that reads %q", h.out, want)
		}
	}
	if log := h.errOut.String(); !strings.Contains(log, "panic while working on exe0002") || !strings.Contains(log, "goroutine") {
		t.Errorf("the stack of the panic was not written to standard error:\n%s", log)
	}
}

// A panic on one node while the work for another asks a question on the
// terminal, a password for a credential, say, waits until the question has
// been answered: its stack goes through the display's Lines, and never
// lands inside the question.
func TestAPanicWaitsForTheQuestionAnotherNodeAsks(t *testing.T) {
	c := fakeDisplays(t)
	asking, stacked := make(chan struct{}), make(chan struct{})
	var tty *terminal
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		switch tg.Name {
		case "exe0002":
			<-asking
			defer close(stacked)
			panic("index out of range [3] with length 3")
		case "exe0001":
			c.mu.Lock()
			shown := c.last.(interface {
				Suspend()
				Resume()
			})
			c.mu.Unlock()
			shown.Suspend()
			_, _ = io.WriteString(tty, "Password for bmc@exe0001: ")
			close(asking)
			<-stacked
			// Long enough for the other worker to have written its stack,
			// which it does as its panic unwinds.
			time.Sleep(100 * time.Millisecond)
			_, _ = io.WriteString(tty, "\n")
			shown.Resume()
		}
		return &transport.Result{Target: tg, Stdout: "Dell|R650|Dell|0A1B|Dell|2.1|2026-01-01|MT4123\n"}, nil
	}}
	tty = &terminal{}
	opts := harnessOptions{recorder: rec, unwatched: true}
	opts.streams = func(s *app.Streams) {
		s.Out, s.Err = tty, tty
		onTerminal(true)(s)
	}
	_, cmd := build(t, opts, "--progress", "counter", "node", "hw", "-n", "exe[0001-0002]")
	cmd.SetOut(tty)
	cmd.SetErr(tty)
	wantCode(t, cmd.Execute(), exitcode.TargetFailed)

	got := tty.String()
	question := strings.Index(got, "Password for bmc@exe0001: \n")
	stack := strings.Index(got, "clusterctl: panic while working on exe0002")
	if question < 0 || stack < question {
		t.Errorf("the stack was not written after the question was answered:\n%s", got)
	}
}
