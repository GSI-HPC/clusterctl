// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package fanout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/clikit/fanout"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// Failure counts and names the items that failed, with the noun given, as
// a node set when every name is one host name, and as the list it was
// given otherwise; it keeps the errors underneath, and its class is the
// targets' or, when the interrupt ended them, canceled.
func TestFailure(t *testing.T) {
	t.Parallel()

	refused := errors.New("exe2: connection refused")
	for _, tc := range []struct {
		name        string
		noun        string
		names       []string
		errs        []error
		interrupted bool
		want        string
		class       progress.Class
	}{
		{"none failed", "hosts", nil, nil, false, "", progress.ClassNone},
		{"hosts", "hosts", []string{"exe3", "exe1", "exe2"}, []error{nil, nil, refused}, false,
			"3 of 5 hosts failed: exe[1-3]", progress.ClassTarget},
		{"no noun", "", []string{"exe2"}, []error{refused}, false, "1 of 5 failed: exe2", progress.ClassTarget},
		{"names that are no host names", "volumes", []string{"config volume", "exe1"}, []error{refused, nil}, false,
			"2 of 5 volumes failed: config volume,exe1", progress.ClassTarget},
		{"a name no node set holds", "", []string{"exe1", "exe99999999999999999999999"}, []error{refused, nil}, false,
			"2 of 5 failed: exe1,exe99999999999999999999999", progress.ClassTarget},
		{"a name that is a group", "", []string{"@compute"}, []error{refused}, false, "1 of 5 failed: @compute", progress.ClassTarget},
		{"interrupted", "hosts", []string{"exe1", "exe2"}, []error{context.Canceled, refused}, true,
			"2 of 5 hosts failed: exe[1-2]", progress.ClassCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := fanout.Failure(tc.noun, 5, tc.names, tc.errs, tc.interrupted)
			if tc.want == "" {
				if err != nil {
					t.Errorf("Failure = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Failure = %v, want %q", err, tc.want)
			}
			if got := progress.Classify(err, nil); got != tc.class {
				t.Errorf("class %s, want %s", got, tc.class)
			}
			for _, e := range tc.errs {
				if e != nil && !errors.Is(err, e) {
					t.Errorf("%v is not kept underneath", e)
				}
			}
		})
	}
}
