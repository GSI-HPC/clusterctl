// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package exitcode_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

func TestFrom(t *testing.T) {
	t.Parallel()

	plain := errors.New("boom")
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, exitcode.OK},
		{"uncoded error is a target failure", plain, exitcode.TargetFailed},
		{"coded error keeps its code", exitcode.Errorf(exitcode.Usage, "bad flag"), exitcode.Usage},
		{"wrapped coded error keeps its code", fmt.Errorf("context: %w", exitcode.Wrap(exitcode.Transport, plain)), exitcode.Transport},
		{"wrap of nil stays nil", exitcode.Wrap(exitcode.Transport, nil), exitcode.OK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitcode.From(tc.err); got != tc.want {
				t.Errorf("From(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorUnwrapsToCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection refused")
	err := exitcode.Wrap(exitcode.Transport, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause) = false, want true", err)
	}
	if got, want := err.Error(), "connection refused"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestWorst covers the exit code rule for several hosts, which #90 found
// implemented five times with different orders.
func TestWorst(t *testing.T) {
	t.Parallel()

	var (
		refused     = errors.New("exe0001: command exited 1")
		usage       = exitcode.Errorf(exitcode.Usage, "exe0002: no service processor")
		unreachable = exitcode.Wrap(exitcode.Transport, errors.New("exe0003: Connection refused"))
		interrupted = fmt.Errorf("exe0004: %w", context.Canceled)
	)
	tests := []struct {
		name string
		errs []error
		want int
	}{
		{"nothing failed", []error{nil, nil}, exitcode.OK},
		{"no errors at all", nil, exitcode.OK},
		{"one refused", []error{nil, refused}, exitcode.TargetFailed},
		{"refused and a configuration problem", []error{refused, usage}, exitcode.Usage},
		{"refused and unreachable", []error{refused, unreachable}, exitcode.Transport},
		{"unreachable and a configuration problem", []error{usage, unreachable}, exitcode.Transport},
		{"interrupted and unreachable", []error{unreachable, interrupted}, exitcode.Interrupted},
		{"a coded interrupt", []error{refused, exitcode.Errorf(exitcode.Interrupted, "not confirmed")}, exitcode.Interrupted},
		{"a cancellation wrapped with another code", []error{exitcode.Wrap(exitcode.Transport, interrupted)}, exitcode.Interrupted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitcode.Worst(tc.errs...); got != tc.want {
				t.Errorf("Worst(%v) = %d, want %d", tc.errs, got, tc.want)
			}
		})
	}
}
