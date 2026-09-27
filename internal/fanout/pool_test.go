// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// nodes names n nodes, exe1 to exeN.
func nodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("exe%d", i+1)
	}
	return out
}

// byExitCode has a test's Bus class errors by the exit code they ask for,
// as the Bus of a command does.
var byExitCode = progresstest.Classify(exitcode.Class)

// A step says why it failed as the exit code of its failures does, not as
// the first of them that says a class of its own: a timeout among hosts
// that could not be reached makes the step's code Transport.
func TestMapStepSaysTheClassOfItsExitCode(t *testing.T) {
	t.Parallel()

	ctx, tree := progresstest.Watch(context.Background(), t, byExitCode)
	fanout.Map(ctx, nodes(3), fanout.Options[string]{Step: "copy", Limit: 1},
		func(_ context.Context, node string) (struct{}, error) {
			switch node {
			case "exe1":
				return struct{}{}, exitcode.Wrap(exitcode.Transport, fmt.Errorf("exe1: %w", context.DeadlineExceeded))
			case "exe2":
				return struct{}{}, exitcode.Errorf(exitcode.Transport, "exe2: connection refused")
			}
			return struct{}{}, nil
		})
	want := `step copy total=3 limit=1 [fold]: failed (transport): 2 of 3 failed: exe[1-2]
  target exe1: failed (timeout): {}: context deadline exceeded
  target exe2: failed (transport): {}: connection refused
  target exe3: ok
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// The executor reports its targets through Map: under the step its caller
// names, or "run", with the flags it is given. A command that exited
// non-zero without an error is a failed target all the same.
func TestExecutorReportsItsTargets(t *testing.T) {
	t.Parallel()

	ctx, tree := progresstest.Watch(context.Background(), t, byExitCode)
	rec := &transport.Recorder{ByTarget: map[string]*transport.Result{"exe2": {ExitCode: 1}}}
	e := &fanout.Executor{Runner: rec, Max: 2, Flags: progress.ShowLines}
	results := e.Run(ctx, targets("exe1", "exe2", "exe3"), transport.Request{Argv: []string{"uptime"}})
	if got := len(fanout.Failures(results)); got != 1 {
		t.Errorf("%d targets failed, want 1", got)
	}
	want := `step run total=3 limit=2 [fold,show-lines]: failed (target): 1 of 3 failed: exe2
  target exe2 [show-lines]: failed (target): {}: command exited 1
  target exe[1,3] [show-lines]: ok
`
	if got := tree(); got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

// The error a fan-out exits with counts and names the hosts that failed,
// keeps the exit code the worst of them asks for, and the progress class of
// that code, and keeps their errors underneath.
func TestFailureError(t *testing.T) {
	t.Parallel()

	down := exitcode.Wrap(exitcode.Transport, errors.New("exe2: Connection refused"))
	for _, tc := range []struct {
		name    string
		results []*transport.Result
		want    string
		code    int
		class   progress.Class
	}{
		{"none failed", []*transport.Result{{Target: transport.Target{Name: "exe1"}}}, "", exitcode.OK, progress.ClassNone},
		{"a command that exited non-zero", []*transport.Result{
			{Target: transport.Target{Name: "exe1"}},
			{Target: transport.Target{Name: "exe2"}, ExitCode: 1},
		}, "1 of 2 hosts failed: exe2", exitcode.TargetFailed, progress.ClassTarget},
		{"an unreachable host among them", []*transport.Result{
			{Target: transport.Target{Name: "exe1"}, ExitCode: 1, Err: errors.New("exe1: command exited 1")},
			{Target: transport.Target{Name: "exe2"}, ExitCode: 255, Err: down},
			{Target: transport.Target{Name: "exe3"}},
		}, "2 of 3 hosts failed: exe[1-2]", exitcode.Transport, progress.ClassTransport},
		{"a timeout among them", []*transport.Result{
			{Target: transport.Target{Name: "exe1"}, ExitCode: 124, Err: fmt.Errorf("exe1: %w", context.DeadlineExceeded)},
			{Target: transport.Target{Name: "exe2"}, ExitCode: 255, Err: down},
		}, "2 of 2 hosts failed: exe[1-2]", exitcode.Transport, progress.ClassTransport},
		{"an interrupt", []*transport.Result{
			{Target: transport.Target{Name: "exe1"}, ExitCode: 255, Err: down},
			{Target: transport.Target{Name: "exe2"}, ExitCode: -1, Err: context.Canceled},
		}, "2 of 2 hosts failed: exe[1-2]", exitcode.Interrupted, progress.ClassCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := fanout.FailureError(tc.results)
			if got := fmt.Sprint(err); tc.want != "" && got != tc.want || tc.want == "" && err != nil {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
			if got := exitcode.From(err); got != tc.code {
				t.Errorf("exit code %d, want %d", got, tc.code)
			}
			if got := progress.Classify(err, exitcode.Class); got != tc.class {
				t.Errorf("class %s, want %s", got, tc.class)
			}
			if tc.code == exitcode.Transport && !errors.Is(err, down) {
				t.Errorf("the error of the unreachable host is not kept underneath: %v", err)
			}
		})
	}
}
