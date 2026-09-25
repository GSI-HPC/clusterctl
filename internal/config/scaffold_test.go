// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/config"
)

// resolveScaffold writes a scaffold into a directory of its own and resolves
// it the way a command would, with nothing from the environment.
func resolveScaffold(t *testing.T, opts config.ScaffoldOptions) (*config.Resolved, []config.ScaffoldFile) {
	t.Helper()
	files, err := config.Scaffold(opts)
	if err != nil {
		t.Fatalf("Scaffold failed: %v", err)
	}
	dir := t.TempDir()
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := config.ExpandEntries([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.Load(paths)
	if err != nil {
		t.Fatalf("loading the scaffold: %v", err)
	}
	r, err := b.Resolve(config.ResolveOptions{Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("resolving the scaffold: %v", err)
	}
	return r, files
}

func TestScaffoldResolvesOnItsOwn(t *testing.T) {
	r, files := resolveScaffold(t, config.ScaffoldOptions{})

	var got []string
	for _, f := range files {
		got = append(got, f.Name+" "+f.Kind+" "+f.DocName)
	}
	want := []string{
		"cluster.yaml Cluster cluster1",
		"config.yaml Config ",
		"inventory.yaml NodeInventory example",
		"site.yaml Site example",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("files =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if got, want := r.Context.Name, "cluster1"; got != want {
		t.Errorf("context = %q, want %q", got, want)
	}
	if got, want := r.SiteName, "example"; got != want {
		t.Errorf("site = %q, want %q", got, want)
	}
	if got, want := r.Spec.Hosts["login"].Host, "login.hpc.example.org"; got != want {
		t.Errorf("hosts.login.host = %q, want %q", got, want)
	}
	if got, want := r.Spec.Slurm.Role, "login"; got != want {
		t.Errorf("slurm.role = %q, want %q", got, want)
	}
	// No user was given, so none is set and ssh keeps deciding.
	if got := r.Context.User; got != "" {
		t.Errorf("context user = %q, want none", got)
	}
}

func TestScaffoldWritesTheValuesGiven(t *testing.T) {
	r, _ := resolveScaffold(t, config.ScaffoldOptions{
		Site:    "lab",
		Cluster: "alpha",
		Domain:  "hpc.lab.example",
		User:    "alice_adm",
	})

	if got, want := r.Context.Name, "alpha"; got != want {
		t.Errorf("context = %q, want %q", got, want)
	}
	if got, want := r.ClusterName, "alpha"; got != want {
		t.Errorf("cluster = %q, want %q", got, want)
	}
	if got, want := r.SiteName, "lab"; got != want {
		t.Errorf("site = %q, want %q", got, want)
	}
	if got, want := r.Context.User, "alice_adm"; got != want {
		t.Errorf("context user = %q, want %q", got, want)
	}
	if got, want := r.Spec.Domains["hpc"], "hpc.lab.example"; got != want {
		t.Errorf("domains.hpc = %q, want %q", got, want)
	}
	// The login node follows the domain unless it is named.
	if got, want := r.Spec.Hosts["login"].Host, "login.hpc.lab.example"; got != want {
		t.Errorf("hosts.login.host = %q, want %q", got, want)
	}

	r, _ = resolveScaffold(t, config.ScaffoldOptions{Login: "gw01.example.org"})
	if got, want := r.Spec.Hosts["login"].Host, "gw01.example.org"; got != want {
		t.Errorf("hosts.login.host = %q, want %q", got, want)
	}
}

// TestScaffoldKeepsNamesThatLookLikeOtherTypes guards the quoting: written
// bare, each of these would be read back as a number or a boolean and the
// document would not validate.
func TestScaffoldKeepsNamesThatLookLikeOtherTypes(t *testing.T) {
	for _, name := range []string{"1", "0600", "1.0", "yes", "off", "null", "1_000", "0x1F", "1e3"} {
		t.Run(name, func(t *testing.T) {
			r, _ := resolveScaffold(t, config.ScaffoldOptions{Site: name, Cluster: name})
			if r.Context.Name != name || r.ClusterName != name || r.SiteName != name {
				t.Errorf("context %q, cluster %q, site %q; want %q for each",
					r.Context.Name, r.ClusterName, r.SiteName, name)
			}
		})
	}
}

func TestScaffoldRejectsWhatCannotBeAName(t *testing.T) {
	tests := []struct {
		opts config.ScaffoldOptions
		want string
	}{
		{config.ScaffoldOptions{Site: "my site"}, "site name"},
		{config.ScaffoldOptions{Site: "-x"}, "site name"},
		{config.ScaffoldOptions{Cluster: "a:b"}, "cluster name"},
		{config.ScaffoldOptions{Domain: "hpc.example.org."}, "not a DNS name"},
		{config.ScaffoldOptions{Domain: "hpc..example.org"}, "not a DNS name"},
		{config.ScaffoldOptions{Domain: "{domains.hpc}"}, "not a DNS name"},
		{config.ScaffoldOptions{Login: "login host"}, "not a host name"},
		{config.ScaffoldOptions{Login: "-oProxyCommand=x"}, "not a host name"},
		{config.ScaffoldOptions{User: "-l"}, "not a user name"},
		{config.ScaffoldOptions{User: "alice adm"}, "not a user name"},
		{config.ScaffoldOptions{User: "alice@EXAMPLE.ORG"}, "not a user name"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			_, err := config.Scaffold(tc.opts)
			if err == nil {
				t.Fatalf("Scaffold(%+v) succeeded", tc.opts)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestScaffoldPointsEditorsAtTheSchema keeps the comment that lets an editor
// check a file naming the schema of the kind the file holds.
func TestScaffoldPointsEditorsAtTheSchema(t *testing.T) {
	files, err := config.Scaffold(config.ScaffoldOptions{})
	if err != nil {
		t.Fatalf("Scaffold failed: %v", err)
	}
	for _, f := range files {
		want := "# yaml-language-server: $schema=https://gsi-hpc.github.io/clusterctl/schema/v1alpha1/" +
			strings.ToLower(f.Kind) + ".json\n"
		if !strings.Contains(string(f.Data), want) {
			t.Errorf("%s does not point at the %s schema", f.Name, f.Kind)
		}
		if !v1alpha1.KnownKind(f.Kind) {
			t.Errorf("%s holds unknown kind %q", f.Name, f.Kind)
		}
		// The template carries the licence of this repository; the file
		// it produces belongs to whoever writes it.
		if strings.Contains(string(f.Data), "SPDX") {
			t.Errorf("%s carries the licence header of the template", f.Name)
		}
	}
}

// Review 9.11: a name that a YAML 1.2 reader such as yq takes for a number,
// 1e3 or 08, is written quoted, and so is every other name, so that no
// reader has to guess.
func TestScaffoldQuotesEveryName(t *testing.T) {
	files, err := config.Scaffold(config.ScaffoldOptions{Site: "08", Cluster: "1e3", User: "007"})
	if err != nil {
		t.Fatalf("Scaffold failed: %v", err)
	}
	want := map[string][]string{
		"config.yaml":    {`currentContext: "1e3"`, `name: "1e3"`, `cluster: "1e3"`, `user: "007"`},
		"cluster.yaml":   {`name: "1e3"`, `site: "08"`, `- "08"`},
		"inventory.yaml": {`name: "08"`},
		"site.yaml":      {`name: "08"`},
	}
	for _, f := range files {
		for _, w := range want[f.Name] {
			if !strings.Contains(string(f.Data), w) {
				t.Errorf("%s does not contain %s:\n%s", f.Name, w, f.Data)
			}
		}
	}
}
