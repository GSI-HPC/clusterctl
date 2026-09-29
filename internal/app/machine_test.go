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
	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
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

// withInventory builds the app over the example configuration with the
// entries of nodes in its inventory, after the example's protected hosts,
// instead of the example's own.
func withInventory(t *testing.T, nodes string) *app.App {
	t.Helper()
	dir := configtest.CopyDir(t, exampleDir, "inventory.yaml", "workstation.yaml")
	inventory := `apiVersion: clusterctl/v1alpha1
kind: NodeInventory
metadata:
  name: example
spec:
  nodes:
    # The protected hosts of the example.
    - nodes: wlm01,dbm01
` + nodes
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
	return a
}

// One node's alias never takes over another node's name. An address two
// inventory entries share cannot be written at all: the inventory refuses it,
// which internal/inventory tests.
func TestSelectKeepsAnAmbiguousNameAsWritten(t *testing.T) {
	t.Parallel()
	a := withInventory(t, `    - nodes: exe0003
      bmcAddress: exe0004
    - nodes: exe0004
`)
	for expr, want := range map[string]string{"exe0004": "exe0004"} {
		ns, err := a.Select(expr)
		if err != nil {
			t.Fatalf("Select(%q) failed: %v", expr, err)
		}
		if got := ns.String(); got != want {
			t.Errorf("Select(%q) = %q, want %q", expr, got, want)
		}
	}
}

// A node's own name written another way, with other padding or as its host
// name, that is another node's bmcAddress as well could be either machine.
// exe0003's bmcAddress exe4 took exe4 from exe0004: a power action typed as
// exe4 reached exe0003, and one typed as EXE04 reached exe0004. Such a name
// is refused now. The name the inventory writes stays its node's, and a
// spelling nothing else answers to is still the node.
func TestSelectRefusesANameThatIsOneNodeAndAnothersAlias(t *testing.T) {
	t.Parallel()
	a := withInventory(t, `    - nodes: exe0003
      bmcAddress: exe4
    - nodes: exe0005
      bmcAddress: exe0006.hpc.example.org
    - nodes: exe[0004,0006]
`)
	for _, tc := range []struct{ expr, want string }{
		{"exe4", ""},
		{"EXE4.", ""},
		{"exe4.hpc.example.org", ""},
		{"exe0006.hpc.example.org", ""},
		{"exe0004", "exe0004"},
		{"EXE04", "exe0004"},
		{"exe6.hpc.example.org", "exe0006"},
		{"exe0003.mgmt.hpc.example.org", "exe0003"},
	} {
		ns, err := a.Select(tc.expr)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("Select(%q) = %q, want it refused as ambiguous", tc.expr, ns)
		case tc.want == "" && exitcode.From(err) != exitcode.Usage:
			t.Errorf("Select(%q): err = %v, want a usage error", tc.expr, err)
		case tc.want == "" && !strings.Contains(err.Error(), "ambiguous"):
			t.Errorf("Select(%q): err = %v, want it to say the name is ambiguous", tc.expr, err)
		case tc.want != "" && err != nil:
			t.Errorf("Select(%q) failed: %v", tc.expr, err)
		case tc.want != "" && ns.String() != tc.want:
			t.Errorf("Select(%q) = %q, want %q", tc.expr, ns, tc.want)
		}
	}
}
