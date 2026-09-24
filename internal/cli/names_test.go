// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// injectedOption is the node name the review used to make ssh run a command
// on the workstation: without a user in the context, the destination is the
// bare node name, and ssh reads a name beginning with "-" as an option.
const injectedOption = "-oProxyCommand=touch${IFS}/tmp/pwned1;#"

func TestNodeNamesThatAreOptionsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"node", "hw", "-n", injectedOption},
		{"hca", "link", "-n", injectedOption},
		{"--set", "fanout.commandTimeout=0s", "exec", "-y", "-n", injectedOption, "--", "uptime"},
		{"exec", "-y", "-n", "exe0001,-v", "--", "uptime"},
	} {
		h, err := run(t, harnessOptions{}, args...)
		if err == nil {
			t.Errorf("%v was accepted", args)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("%v: exit code = %d, want %d (%v)", args, got, want, err)
		}
		if !strings.Contains(err.Error(), "not a host name") {
			t.Errorf("%v: error = %q, want it to say the name is not a host name", args, err)
		}
		if calls := h.recorder.Calls(); len(calls) != 0 {
			t.Errorf("%v: sent %v", args, h.recorder.Commands())
		}
	}
}

// listener stands in for a service on the workstation that someone other
// than the administrator opened, and records every request it receives.
type listener struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   []string
}

func newListener(t *testing.T) *listener {
	t.Helper()
	l := &listener{}
	l.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		l.mu.Lock()
		l.seen = append(l.seen, r.Method+" "+r.URL.Path+" "+user+":"+password)
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"PowerState":"On","Status":{"Health":"OK"}}`))
	}))
	t.Cleanup(l.server.Close)
	return l
}

// port is where the listener accepts connections.
func (l *listener) port() string {
	return l.server.URL[strings.LastIndexByte(l.server.URL, ':')+1:]
}

func (l *listener) requests() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

func TestNodeNamesThatRedirectARedfishRequestAreRefused(t *testing.T) {
	isolateHome(t)
	t.Setenv("BMC_PASSWORD", "hunter2")
	l := newListener(t)
	port := l.port()

	for _, args := range [][]string{
		{"bmc", "status", "-n", "localhost:" + port + "#"},
		{"bmc", "status", "-n", "localhost:" + port + "?"},
		{"bmc", "status", "-n", "localhost:" + port + "/x"},
		{"bmc", "status", "-n", "x@localhost:" + port + "#"},
		{"bmc", "redfish", "get", "/redfish/v1/AccountService/Accounts", "-n", "localhost:" + port + "#"},
	} {
		_, err := run(t, harnessOptions{}, args...)
		if err == nil {
			t.Errorf("%v was accepted", args)
			continue
		}
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("%v: exit code = %d, want %d (%v)", args, got, want, err)
		}
	}
	if seen := l.requests(); len(seen) != 0 {
		t.Errorf("the BMC password went to another host: %q", seen)
	}
}
