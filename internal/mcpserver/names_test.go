// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/cli"
	"github.com/GSI-HPC/clusterctl/internal/mcpserver"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// TestReadCommandRefusesNodeNamesThatAreNotHostNames covers the two ways the
// review found for an agent to reach past the plan and the confirmation
// through read_command: a node name that ssh reads as an option, and one that
// sends a Redfish request, with the site's BMC password, to a port of the
// agent's choosing.
func TestReadCommandRefusesNodeNamesThatAreNotHostNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BMC_PASSWORD", "hunter2")

	var (
		mu   sync.Mutex
		seen []string
	)
	listener := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+user+":"+password)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"PowerState":"On"}`))
	}))
	t.Cleanup(listener.Close)
	port := listener.URL[strings.LastIndexByte(listener.URL, ':')+1:]

	recorder := &transport.Recorder{}
	session := serve(t, recorder)

	for _, args := range [][]string{
		{"node", "hw", "-n", "-oProxyCommand=touch${IFS}/tmp/pwned1;#"},
		{"hca", "link", "-n", "-oProxyCommand=touch${IFS}/tmp/pwned1;#"},
		{"bmc", "status", "-n", "localhost:" + port + "#"},
		{"bmc", "redfish", "get", "/redfish/v1/AccountService/Accounts", "-n", "x@localhost:" + port + "#"},
	} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "read_command",
			Arguments: map[string]any{"args": args},
		})
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !res.IsError {
			t.Errorf("%v was run: %v", args, res.StructuredContent)
			continue
		}
		if msg := text(res); !strings.Contains(msg, "not a host name") {
			t.Errorf("%v: message = %q, want it to say the name is not a host name", args, msg)
		}
	}
	if calls := recorder.Commands(); len(calls) != 0 {
		t.Errorf("ssh was run: %v", calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Errorf("the BMC password went to another host: %q", seen)
	}
}

// serve starts a server over the example configuration and returns a client
// session connected to it.
func serve(t *testing.T, recorder *transport.Recorder) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server, err := mcpserver.New(ctx, mcpserver.Options{
		App: app.Options{
			ConfigFiles: []string{exampleDir},
			Runner:      recorder,
		},
		StateDir: filepath.Join(t.TempDir(), "state"),
		CacheDir: filepath.Join(t.TempDir(), "cache"),
		Command:  cli.CommandTree(recorder),
		Version:  "test",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := server.SDK().Connect(ctx, serverSide, nil); err != nil {
		t.Fatalf("server Connect failed: %v", err)
	}
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("client Connect failed: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
