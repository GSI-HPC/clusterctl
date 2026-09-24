// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package redfish_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/redfish"
)

// TestHostCannotRedirectTheRequest covers the review's finding that a node
// name carrying URL delimiters sent the request, and the BMC password with
// it, to a host and port of the caller's choosing. The listener stands in for
// whoever opened a port on the workstation.
func TestHostCannotRedirectTheRequest(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		seen []string
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Host+r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	hostPort := strings.TrimPrefix(server.URL, "https://")

	for _, host := range []string{
		hostPort,
		hostPort + "#.mgmt.example.org",
		hostPort + "?.mgmt.example.org",
		hostPort + "/x.mgmt.example.org",
		"x@" + hostPort + "#.mgmt.example.org",
		"admin:pw@" + hostPort,
		"[::1]",
		"fe80::1%25lo",
		"",
		"-x",
		"bmc .example.org",
	} {
		c := &redfish.Client{
			Host:     host,
			Username: "admin",
			Password: "hunter2",
			// The real transport, so that nothing but the URL decides where
			// the request goes.
			Verify: false,
		}
		if _, err := c.Get(context.Background(), "/redfish/v1/Systems/1"); err == nil {
			t.Errorf("a request to %q was sent", host)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Errorf("the listener received %q", seen)
	}
}

func TestBaseURLIsTheHostAlone(t *testing.T) {
	t.Parallel()

	for host, want := range map[string]string{
		"bmc1.mgmt.example.org": "https://bmc1.mgmt.example.org",
		"10.0.0.1":              "https://10.0.0.1",
		"fe80::1":               "https://[fe80::1]",
	} {
		c := &redfish.Client{Host: host}
		got, err := c.BaseURL()
		if err != nil {
			t.Errorf("BaseURL of %q: %v", host, err)
			continue
		}
		if got != want {
			t.Errorf("BaseURL of %q = %q, want %q", host, got, want)
		}
	}
}

// TestPathStaysOnTheHost checks that a path, which bmc redfish get takes
// from its caller, cannot name another host either.
func TestPathStaysOnTheHost(t *testing.T) {
	t.Parallel()

	f := newFakeBMC(t, nil)
	c := f.client(t)
	var got []string
	c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.URL.Host)
		return dialOnly(f.server).RoundTrip(r)
	})
	for _, path := range []string{"//evil.example.org/x", "@evil.example.org/x", "/../../x"} {
		_, _, _ = c.DoRaw(context.Background(), http.MethodGet, path, nil)
	}
	for _, host := range got {
		if host != "example.com" {
			t.Errorf("a request went to %q", host)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
