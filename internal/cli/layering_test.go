// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// These tests are the cases of the adversarial review, section 9, in which a
// layer of the configuration took a protection away without a word.

// writeFiles writes files into a new directory and returns it.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// copyExample copies the example configuration into a new directory, leaving
// out the files named, and returns it.
func copyExample(t *testing.T, leaveOut ...string) string {
	t.Helper()
	items, err := os.ReadDir(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, item := range items {
		if item.IsDir() || slices.Contains(leaveOut, item.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(exampleDir, item.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, item.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// contextOverride is a Config document that gives context cluster2 the
// overrides written in body, indented under overrides.
func contextOverride(body string) string {
	return "apiVersion: clusterctl/v1alpha1\nkind: Config\ncontexts:\n" +
		"  - name: cluster2\n    cluster: cluster2\n    user: alice_adm\n    overrides:\n" + body
}

// wantProtected runs a dry run power off of the protected host wlm01 and
// fails unless it is refused.
func wantProtected(t *testing.T, opts harnessOptions, args ...string) {
	t.Helper()
	args = append(args, "bmc", "power", "off", "-n", "wlm01", "--dry-run")
	h, err := run(t, opts, args...)
	if err == nil {
		t.Fatalf("the protected host wlm01 was not refused:\n%s", h.out)
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d (%v)", got, want, err)
	}
}

// Review 9.1: an override in the nested form merges key by key, like every
// other mapping, instead of replacing the whole section.
func TestOverrideMappingMergesKeyByKey(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"override.yaml": contextOverride("      safety: {confirmAbove: 4}\n"),
	})
	opts := harnessOptions{config: []string{dir}}

	// The protected hosts of the site survive the override.
	wantProtected(t, opts, "--context", "cluster2")

	h, err := run(t, opts, "--context", "cluster2", "config", "explain", "safety.confirmAbove")
	if err != nil {
		t.Fatalf("config explain failed: %v", err)
	}
	for _, want := range []string{"4", "context", "override.yaml:"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("config explain does not mention %q:\n%s", want, h.out)
		}
	}
	h, err = run(t, opts, "--context", "cluster2", "config", "explain", "safety.protectedHosts")
	if err != nil {
		t.Fatalf("config explain failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "[wlm01,dbm01]") {
		t.Errorf("the protected hosts were lost:\n%s", h.out)
	}

	// --set merges the same way.
	wantProtected(t, harnessOptions{}, "--set", "safety={confirmAbove: 4}")
	h, err = run(t, harnessOptions{}, "--set", "safety={confirmAbove: 4}", "config", "view", "-o", "json")
	if err != nil {
		t.Fatalf("config view failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "protectedHosts") || !strings.Contains(h.out.String(), "powerOnBatch") {
		t.Errorf("config view lost the rest of the safety section:\n%s", h.out)
	}
}

// Review 9.1: an override value is checked against the schema before it is
// applied, so a null cannot erase a section.
func TestOverrideValueIsValidated(t *testing.T) {
	for name, body := range map[string]string{
		"null section":  "      safety: null\n",
		"wrong type":    "      safety.confirmAbove: many\n",
		"list for map":  "      safety: [wlm01]\n",
		"nested typo":   "      safety: {protectedHostz: []}\n",
		"scalar in map": "      fanout: 4\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"override.yaml": contextOverride(body)})
			_, err := run(t, harnessOptions{config: []string{dir}}, "config", "validate")
			if err == nil {
				t.Fatal("config validate accepted the override")
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}
			if !strings.Contains(err.Error(), "override.yaml:") {
				t.Errorf("error = %v, want it to name the file and line", err)
			}
		})
	}
}

// Review 9.2: an override key that differs from a field only in case is
// refused, not matched without regard to case or dropped.
func TestOverrideKeysAreCaseSensitive(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"override.yaml": contextOverride("      safety.protectedhosts: []\n      fanout.Max: 2\n"),
	})
	// Every context is checked, not only the current one.
	_, err := run(t, harnessOptions{config: []string{dir}}, "config", "validate")
	if err == nil {
		t.Fatal("config validate accepted keys that differ only in case")
	}
	for _, want := range []string{"override.yaml:", `did you mean "protectedHosts"`, `did you mean "max"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}

	_, err = run(t, harnessOptions{}, "--set", "safety.ProtectedHosts=[exe0001]", "config", "validate")
	if err == nil {
		t.Fatal("--set accepted a key that differs only in case")
	}
	if !strings.Contains(err.Error(), `--set`) || !strings.Contains(err.Error(), `did you mean "protectedHosts"`) {
		t.Errorf("error = %v, want it to name --set and suggest the field", err)
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}

	// The unknown path of an override is reported where it was written,
	// with the field that was probably meant.
	dir = writeFiles(t, map[string]string{
		"override.yaml": contextOverride("      safety.confirmAbov: 2\n"),
	})
	_, err = run(t, harnessOptions{config: []string{dir}}, "config", "validate")
	if err == nil || !strings.Contains(err.Error(), "override.yaml:") || !strings.Contains(err.Error(), `did you mean "confirmAbove"`) {
		t.Errorf("error = %v, want the position and a suggestion", err)
	}
}

// Review 9.3: a second document of the same kind and name is an error that
// names both, rather than replacing the first.
func TestDuplicateDocumentsAreRefused(t *testing.T) {
	dir := copyExample(t)
	site, err := os.ReadFile(filepath.Join(dir, "site.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Replace(string(site), "      - wlm01\n", "", 1)
	if err := os.WriteFile(filepath.Join(dir, "site_old.yaml"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate")
	if err == nil {
		t.Fatal("config validate accepted two Site documents named example")
	}
	for _, want := range []string{"site_old.yaml:", "site.yaml:", `Site "example"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	wantProtected(t, harnessOptions{bare: true, config: []string{dir}})
}

// Review 9.3: only a later Config document may redefine a context; one
// document that names a context twice is a mistake.
func TestContextNamedTwiceInOneDocumentIsRefused(t *testing.T) {
	dir := writeFiles(t, map[string]string{"extra.yaml": "apiVersion: clusterctl/v1alpha1\nkind: Config\ncontexts:\n" +
		"  - name: lab\n    cluster: cluster1\n  - name: lab\n    cluster: cluster2\n"})
	_, err := run(t, harnessOptions{config: []string{dir}}, "config", "validate")
	if err == nil || !strings.Contains(err.Error(), `a second context "lab"`) {
		t.Errorf("err = %v, want the second context reported", err)
	}
}

// Review 9.6: a cluster written by config init takes the nodes of its own
// site only, also when another site is loaded with it.
func TestConfigInitPinsTheInventory(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	if _, err := run(t, harnessOptions{bare: true, config: []string{a}},
		"config", "init", a, "--site", "sitea", "--cluster", "alpha", "--domain", "a.example.org"); err != nil {
		t.Fatalf("config init failed: %v", err)
	}
	if _, err := run(t, harnessOptions{bare: true, config: []string{b}},
		"config", "init", b, "--site", "siteb", "--cluster", "beta", "--domain", "b.example.org"); err != nil {
		t.Fatalf("config init failed: %v", err)
	}
	// Both scaffolds have a Config document; the second one's context
	// names differ, so they add up. Site b gets compute nodes.
	inv := filepath.Join(b, "inventory.yaml")
	data, err := os.ReadFile(inv)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "  nodes: []",
		"  nodes:\n    - nodes: gpu[01-04]\n      attributes: {class: compute}", 1))
	if err := os.WriteFile(inv, data, 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := run(t, harnessOptions{bare: true, config: []string{a, b}}, "--context", "beta", "node", "fqdn", "-n", "@compute")
	if err != nil {
		t.Fatalf("node fqdn on beta failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "gpu[01-04].b.example.org"; got != want {
		t.Errorf("beta: node fqdn = %q, want %q", got, want)
	}
	h, err = run(t, harnessOptions{bare: true, config: []string{a, b}}, "--context", "alpha", "node", "fqdn", "-n", "@compute")
	if err == nil {
		t.Errorf("alpha selected the nodes of site b: %s", h.out)
	}
}

// Review 9.8: config init without DIR does not write a second configuration
// next to one the search path reads already, such as a team's in
// /etc/clusterctl.
func TestConfigInitRefusesToShadowTheSearchPath(t *testing.T) {
	user := isolateHome(t)
	team := copyExample(t)
	saved := configDirs
	configDirs = func() []string { return []string{team, user} }
	t.Cleanup(func() { configDirs = saved })

	_, err := run(t, harnessOptions{bare: true}, "config", "init")
	if err == nil {
		t.Fatal("config init wrote a configuration that shadows the team's")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(err.Error(), team) || !strings.Contains(err.Error(), "config init DIR") {
		t.Errorf("error = %v, want it to name %s and the way out", err, team)
	}
	if _, err := os.Stat(user); err == nil {
		t.Errorf("%s was created although config init refused", user)
	}

	// A search path directory that holds no configuration is no reason
	// to refuse.
	configDirs = func() []string { return []string{filepath.Join(t.TempDir(), "missing"), user} }
	if _, err := run(t, harnessOptions{bare: true}, "config", "init"); err != nil {
		t.Fatalf("config init failed with nothing else on the search path: %v", err)
	}
}

// Review 9.10: config explain reports the value the commands use and the
// line a context wrote it on.
func TestConfigExplainCoversFlagsAndContexts(t *testing.T) {
	h, err := run(t, harnessOptions{}, "--fanout", "4", "config", "explain", "fanout.max")
	if err != nil {
		t.Fatalf("config explain failed: %v", err)
	}
	for _, want := range []string{"4", "flags", "--fanout"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("config explain does not mention %q:\n%s", want, h.out)
		}
	}

	for _, path := range []string{"fanout.max", "defaultUser"} {
		h, err = run(t, harnessOptions{}, "--context", "cluster2", "config", "explain", path)
		if err != nil {
			t.Fatalf("config explain %s failed: %v", path, err)
		}
		for _, want := range []string{"context", "config.yaml:"} {
			if !strings.Contains(h.out.String(), want) {
				t.Errorf("config explain %s does not mention %q:\n%s", path, want, h.out)
			}
		}
	}
}

// Review 9.11: a key with a dot in it, such as a static group rack.R01,
// stays one key when the layers are merged.
func TestDottedKeysStayOneKey(t *testing.T) {
	dir := copyExample(t)
	cluster := filepath.Join(dir, "cluster.yaml")
	data, err := os.ReadFile(cluster)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(data), "          infra: wlm01,dbm01\n",
		"          infra: wlm01,dbm01\n          rack.R01: exe[0001-0002]\n", 1)
	if patched == string(data) {
		t.Fatal("the example cluster has no static infra group to add to")
	}
	if err := os.WriteFile(cluster, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "explain", "groups.sources.static.static.rack.R01")
	if err != nil {
		t.Fatalf("config explain failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "exe[0001-0002]") || !strings.Contains(h.out.String(), "cluster.yaml:") {
		t.Errorf("config explain does not show the group:\n%s", h.out)
	}
}

// Review 9.11: use-context prints lines that can be pasted as they are.
func TestConfigUseContextQuotesTheName(t *testing.T) {
	dir := writeFiles(t, map[string]string{"extra.yaml": "apiVersion: clusterctl/v1alpha1\nkind: Config\n" +
		"contexts:\n  - name: \"it's; yes\"\n    cluster: cluster1\n"})
	h, err := run(t, harnessOptions{config: []string{dir}}, "config", "use-context", "it's; yes")
	if err != nil {
		t.Fatalf("config use-context failed: %v", err)
	}
	for _, want := range []string{
		`export CLUSTERCTL_CONTEXT='it'\''s; yes'`,
		`clusterctl --context 'it'\''s; yes' ...`,
		`currentContext: "it's; yes"`,
	} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, h.out)
		}
	}
}

// Review 9.11: config init reports a directory it cannot use as a usage
// error, not as a failed target.
func TestConfigInitFailsWithUsageLocally(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, harnessOptions{bare: true}, "config", "init", filepath.Join(file, "sub"))
	if err == nil {
		t.Fatal("config init wrote below a regular file")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d (%v)", got, want, err)
	}
}
