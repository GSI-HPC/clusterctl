// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package redfish_test

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// Every request is a call that says what was asked of which processor, how
// it answered, and in a word why it failed: a refused account, a refusal, a
// changed certificate, a processor that is not there, an interrupt.
func TestEveryRequestIsReportedAsACall(t *testing.T) {
	t.Parallel()
	f := newFakeBMC(t, nil)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := closed.Addr().String()
	_ = closed.Close()

	for _, tc := range []struct {
		name   string
		client func(t *testing.T) *redfish.Client
		path   string
		cancel bool
		want   string
	}{
		{"an answer", f.client, "/redfish/v1/Systems/1", false,
			"call redfish host=example.com method=GET path=/redfish/v1/Systems/1 http=200: ok\n"},
		{"a refused account", func(t *testing.T) *redfish.Client {
			c := f.client(t)
			c.Password = "wrong"
			return c
		}, "/redfish/v1/Systems/1", false,
			"call redfish host=example.com method=GET path=/redfish/v1/Systems/1 http=401: failed (auth): example.com: 401 Unauthorized: \n"},
		{"a refusal", f.client, "/redfish/v1/broken", false,
			"call redfish host=example.com method=GET path=/redfish/v1/broken http=400: failed (target): example.com: 400 Bad Request: the request failed Unsupported value Use a supported value.\n"},
		{"a changed certificate", func(t *testing.T) *redfish.Client {
			store := &redfish.PinStore{Path: filepath.Join(t.TempDir(), "pins")}
			if err := store.Set(context.Background(), "example.com", "sha256:00"); err != nil {
				t.Fatal(err)
			}
			return f.pinningClient(t, store)
		}, "/redfish/v1/Systems/1", false, ""},
		{"a processor that is not there", func(*testing.T) *redfish.Client {
			return &redfish.Client{Host: "example.com", Username: "admin", Password: "secret",
				Transport: &http.Transport{DialContext: dialTo(gone)}}
		}, "/redfish/v1/Systems/1", false, ""},
		{"an interrupt", f.client, "/redfish/v1/Systems/1", true,
			"call redfish host=example.com method=GET path=/redfish/v1/Systems/1: canceled (canceled): example.com: context canceled\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &progresstest.Capture{}
			bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
			ctx, cancel := context.WithCancel(progress.WithBus(context.Background(), bus))
			defer cancel()
			if tc.cancel {
				cancel()
			}
			_, _, _ = tc.client(t).DoRaw(ctx, http.MethodGet, tc.path, nil)
			bus.Close()
			events := c.Events()
			progresstest.Check(t, events)
			if tc.want != "" {
				if got := c.Tree(); got != tc.want {
					t.Errorf("progress:\n%s\nwant:\n%s", got, tc.want)
				}
				return
			}
			// What the dialer or the handshake said varies; the class
			// does not.
			want := map[string]progress.Class{"a changed certificate": progress.ClassPin,
				"a processor that is not there": progress.ClassTransport}[tc.name]
			var ends []progress.Event
			for _, e := range events {
				if e.Type == progress.TypeEnd {
					ends = append(ends, e)
				}
			}
			if len(ends) != 1 || ends[0].Status != progress.StatusFailed || ends[0].Class != want || ends[0].HTTPStatus != 0 {
				t.Errorf("the call ended %+v, want one failed %s with no status", ends, want)
			}
		})
	}
}
