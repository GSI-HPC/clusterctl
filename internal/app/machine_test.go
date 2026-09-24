// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

func TestSelectNamesMachinesNotSpellings(t *testing.T) {
	t.Parallel()
	a := newApp(t, &transport.Recorder{})

	tests := []struct{ expr, want string }{
		{"WLM01", "wlm01"},
		{"wlm01.", "wlm01"},
		{"wlm1", "wlm01"},
		{"wlm01.hpc.example.org", "wlm01"},
		{"WLM01.HPC.EXAMPLE.ORG.", "wlm01"},
		{"wlm01.mgmt.hpc.example.org", "wlm01"},
		{"10.0.1.1", "wlm01"},
		{"exe1.hpc.example.org", "exe0001"},
		// Not in the inventory, but the host name the rules give node7.
		{"node7.example.org", "node7"},
		// A domain the rules do not give wlm01 is not known to be it.
		{"wlm01.example.org", "wlm01.example.org"},
		{"10.9.9.9", "10.9.9.9"},
		{"exe0001,EXE1,exe0001.hpc.example.org,10.0.2.1", "exe0001"},
	}
	for _, tc := range tests {
		ns, err := a.Select(tc.expr)
		if err != nil {
			t.Errorf("Select(%q) failed: %v", tc.expr, err)
			continue
		}
		if got := ns.String(); got != tc.want {
			t.Errorf("Select(%q) = %q, want %q", tc.expr, got, tc.want)
		}
	}
}

// An address two inventory entries share names neither machine, and one
// node's alias never takes over another node's name.
func TestSelectKeepsAnAmbiguousNameAsWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"site.yaml", "cluster.yaml", "config.yaml", "secrets.sops.yaml"} {
		data, err := os.ReadFile(filepath.Join(exampleDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inventory := `apiVersion: clusterctl/v1alpha1
kind: NodeInventory
metadata:
  name: example
spec:
  nodes:
    # The protected hosts of the example.
    - nodes: wlm01,dbm01
    - nodes: exe0001
      address: 10.0.2.1
    - nodes: exe0002
      address: 10.0.2.1
    - nodes: exe0003
      bmcAddress: exe0004
    - nodes: exe0004
`
	if err := os.WriteFile(filepath.Join(dir, "inventory.yaml"), []byte(inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(context.Background(), app.Streams{
		In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{},
		StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"),
	}, app.Options{ConfigFiles: []string{dir}, Env: func(string) string { return "" }, Runner: &transport.Recorder{}})
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}
	for expr, want := range map[string]string{"10.0.2.1": "10.0.2.1", "exe0004": "exe0004"} {
		ns, err := a.Select(expr)
		if err != nil {
			t.Fatalf("Select(%q) failed: %v", expr, err)
		}
		if got := ns.String(); got != want {
			t.Errorf("Select(%q) = %q, want %q", expr, got, want)
		}
	}
}
