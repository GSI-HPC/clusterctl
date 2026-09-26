// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package transport_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// runnerFunc adapts a function to transport.Runner.
type runnerFunc func(context.Context, transport.Target, transport.Request) (*transport.Result, error)

func (f runnerFunc) Run(ctx context.Context, t transport.Target, req transport.Request) (*transport.Result, error) {
	return f(ctx, t, req)
}

// watch returns a context whose Bus sends its events to a capture. tree
// closes the Bus, checks every promise the events make and returns their
// tree.
func watch(t *testing.T) (context.Context, func() string) {
	t.Helper()
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	return progress.WithBus(context.Background(), bus), func() string {
		t.Helper()
		bus.Close()
		progresstest.Check(t, c.Events())
		return c.Tree()
	}
}

// Every request is a call, which says where it went and how it ended in a
// word: a dry run's recorded one is skipped, a host that could not be
// reached is the transport's failure, and a command timeout(1) ended ran
// out of time, although all three exit the way they did before.
func TestTracedReportsEveryCall(t *testing.T) {
	target := transport.Target{Name: "exe1", Host: "exe1.example.org", Role: "compute"}
	interrupted := exitcode.Wrap(exitcode.Interrupted, fmt.Errorf("exe1: %w", context.Canceled))
	for _, tc := range []struct {
		name   string
		dryRun bool
		result *transport.Result
		err    error
		want   string
	}{
		{"a command that succeeds", false, &transport.Result{Target: target}, nil,
			"call ssh node=exe1 host=exe1.example.org role=compute timeout=10s exit=0: ok\n"},
		{"a command that exits non-zero", false, transport.ExitResult(target, 3, "", "no such file\n"), nil,
			"call ssh node=exe1 host=exe1.example.org role=compute timeout=10s exit=3: failed (target): exe1 (exe1.example.org): command exited 3\n"},
		{"a host that cannot be reached", false, transport.ExitResult(target, 255, "", "ssh: connect to host exe1: Connection refused\n"), nil,
			"call ssh node=exe1 host=exe1.example.org role=compute timeout=10s exit=255: failed (transport): exe1 (exe1.example.org): ssh: connect to host exe1: Connection refused\n"},
		{"a command timeout(1) ended", false,
			&transport.Result{Target: target, ExitCode: 124, Err: &transport.TimeoutError{Target: target, ExitCode: 124, Timeout: 10 * time.Second}}, nil,
			"call ssh node=exe1 host=exe1.example.org role=compute timeout=10s exit=124: failed (timeout): exe1 (exe1.example.org): command exited 124\n"},
		{"an interrupt", false, &transport.Result{Target: target, ExitCode: -1, Err: interrupted}, nil,
			"call ssh node=exe1 host=exe1.example.org role=compute timeout=10s: canceled (canceled): exe1: context canceled\n"},
		{"a stand-in that fails with no error", false, &transport.Result{Target: target, ExitCode: 2}, nil,
			"call ssh node=exe1 host=exe1.example.org role=compute timeout=10s exit=2: failed (target): exe1 (exe1.example.org): command exited 2\n"},
		{"a request that cannot be run", false, nil, exitcode.Errorf(exitcode.Usage, "exe1: not a host name"),
			"call ssh node=exe1 host=exe1.example.org role=compute timeout=10s: failed (usage): exe1: not a host name\n"},
		{"a dry run's recorded call", true, &transport.Result{Target: target}, nil,
			"call ssh node=exe1 host=exe1.example.org role=compute timeout=10s [dry-run]: skipped: dry run: not sent\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, tree := watch(t)
			var inner *progress.Span
			runner := transport.Traced(runnerFunc(func(ctx context.Context, _ transport.Target, _ transport.Request) (*transport.Result, error) {
				inner = progress.SpanFrom(ctx)
				return tc.result, tc.err
			}), tc.dryRun)
			result, err := runner.Run(ctx, target, transport.Request{Argv: []string{"true"}, Timeout: 10 * time.Second})
			if result != tc.result || !errors.Is(err, tc.err) {
				t.Errorf("Run = %v, %v; want what the runner gave, %v, %v", result, err, tc.result, tc.err)
			}
			if inner == nil {
				t.Error("the runner was not given the call's context")
			}
			if got := tree(); got != tc.want {
				t.Errorf("progress:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// Nobody watching costs nothing: without a Bus the request runs as it
// would untraced, and its context is the caller's.
func TestTracedWithoutABusOnlyRuns(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "caller")
	want := &transport.Result{ExitCode: 1}
	runner := transport.Traced(runnerFunc(func(got context.Context, _ transport.Target, _ transport.Request) (*transport.Result, error) {
		if got != ctx {
			t.Error("the runner was given another context")
		}
		return want, nil
	}), true)
	if result, err := runner.Run(ctx, transport.Target{Name: "exe1"}, transport.Request{}); result != want || err != nil {
		t.Errorf("Run = %v, %v; want %v, nil", result, err, want)
	}
}
