// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// panicLog sends the stacks of recovered panics to a buffer for the rest of
// the test, and returns it.
func panicLog(t *testing.T) *strings.Builder {
	t.Helper()
	var log strings.Builder
	old := fanout.PanicLog
	fanout.PanicLog = &log
	t.Cleanup(func() { fanout.PanicLog = old })
	return &log
}

// A panic in the work for one node ended the whole process, because recover
// only catches a panic in its own goroutine and the fan-out workers had
// none. It is that node's failure now: the others are reported, and the
// command does not exit 0.
func TestAPanicOnOneNodeFailsOnlyThatNode(t *testing.T) {
	log := panicLog(t)
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		if tg.Name == "exe0002" {
			panic("index out of range [3] with length 3")
		}
		return &transport.Result{Target: tg, Stdout: "Dell|R650|Dell|0A1B|Dell|2.1|2026-01-01|MT4123\n"}, nil
	}}
	h, err := run(t, harnessOptions{recorder: rec}, "-o", "json", "node", "hw", "-n", "exe[0001-0003]")
	if got, want := exitcode.From(err), exitcode.TargetFailed; got != want {
		t.Errorf("exit code = %d, want %d (%v)", got, want, err)
	}
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
	if !strings.Contains(log.String(), "exe0002") {
		t.Errorf("the stack of the panic was not written:\n%s", log)
	}
}

// The host key scan runs its own workers, which had no recover either.
func TestAPanicInAHostKeyScanFailsOnlyThatHost(t *testing.T) {
	panicLog(t)
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
}
