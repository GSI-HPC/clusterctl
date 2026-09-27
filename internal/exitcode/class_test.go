// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package exitcode_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// classified is an error that says its own class, as a pin mismatch or a
// refused account does.
type classified struct{ class progress.Class }

func (c classified) Error() string                 { return "classified" }
func (c classified) ProgressClass() progress.Class { return c.class }

// An exit code has the class CodeClass gives it, and a success none.
func TestCodeClass(t *testing.T) {
	t.Parallel()
	for code, want := range map[int]progress.Class{
		exitcode.OK:           progress.ClassNone,
		exitcode.TargetFailed: progress.ClassTarget,
		exitcode.Usage:        progress.ClassUsage,
		exitcode.Transport:    progress.ClassTransport,
		exitcode.Interrupted:  progress.ClassCanceled,
		42:                    progress.ClassTarget,
	} {
		if got := exitcode.CodeClass(code); got != want {
			t.Errorf("CodeClass(%d) = %s, want %s", code, got, want)
		}
	}
}

// With Class as its fallback, progress.Classify tells the class of an
// error by its exit code once its first rules have not.
func TestClassIsTheFallbackOfClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want progress.Class
	}{
		{"an error that says nothing is the target's", errors.New("command exited 1"), progress.ClassTarget},
		{"an exit code of 1", exitcode.Errorf(exitcode.TargetFailed, "refused"), progress.ClassTarget},
		{"unreachable", exitcode.Errorf(exitcode.Transport, "exe0001: no route to host"), progress.ClassTransport},
		{"refused before anything was sent", exitcode.Errorf(exitcode.Usage, "no such node"), progress.ClassUsage},
		{"interrupted by its exit code", exitcode.Errorf(exitcode.Interrupted, "exe0001: interrupted"), progress.ClassCanceled},
		{"an interrupt however wrapped", exitcode.Wrap(exitcode.Transport, fmt.Errorf("exe0001: %w", context.Canceled)), progress.ClassCanceled},
		{"a dial that timed out", exitcode.Wrap(exitcode.Transport, &net.OpError{Op: "dial", Err: context.DeadlineExceeded}), progress.ClassTimeout},
		{"a network error that is no timeout", exitcode.Wrap(exitcode.Transport, &net.OpError{Op: "dial", Err: errors.New("connection refused")}), progress.ClassTransport},
		{"its class before the exit code", exitcode.Wrap(exitcode.Transport, classified{progress.ClassAuth}), progress.ClassAuth},
		{"a class of none leaves the rules", exitcode.Wrap(exitcode.Transport, classified{progress.ClassNone}), progress.ClassTransport},
	}
	for _, tc := range tests {
		if got := progress.Classify(tc.err, exitcode.Class); got != tc.want {
			t.Errorf("%s: Classify(%v, Class) = %s, want %s", tc.name, tc.err, got, tc.want)
		}
	}
}
