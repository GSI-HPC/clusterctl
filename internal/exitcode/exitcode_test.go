// SPDX-License-Identifier: LGPL-3.0-or-later

package exitcode_test

import (
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
