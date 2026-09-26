// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package hostkeys_test

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/hostkeys"
	"github.com/GSI-HPC/clusterctl/internal/progress"
)

// A host whose key could not be collected did not answer, which is what
// the scan commands exit 3 for, and a display reads it the same way: as a
// host that could not be reached, one that ran out of time, or an
// interrupt.
func TestAHostWithoutAKeyDidNotAnswer(t *testing.T) {
	t.Parallel()
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	for _, tc := range []struct {
		name   string
		dial   func(ctx context.Context, network, address string) (net.Conn, error)
		cancel bool
		class  progress.Class
	}{
		{"refused", func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}, false, progress.ClassTransport},
		{"silent", func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, silent.Addr().String())
		}, false, progress.ClassTimeout},
		{"interrupted", func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, silent.Addr().String())
		}, true, progress.ClassCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				time.AfterFunc(50*time.Millisecond, cancel)
			}
			s := &hostkeys.Scanner{Timeout: 200 * time.Millisecond, Dial: tc.dial}
			_, err := s.Scan(ctx, "exe0001")
			if got := exitcode.From(err); got != exitcode.Transport {
				t.Errorf("exit code = %d, want %d (%v)", got, exitcode.Transport, err)
			}
			if got := progress.Classify(err); got != tc.class {
				t.Errorf("class = %s, want %s (%v)", got, tc.class, err)
			}
		})
	}
}
