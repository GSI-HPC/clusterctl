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

// exampleDir is the configuration shipped with the documentation. Loading it
// in the tests keeps the example honest.
const exampleDir = "../../examples/site"

func loadExample(t *testing.T) *config.Bundle {
	t.Helper()
	b, err := config.LoadDefault(func(k string) string {
		if k == config.EnvConfig {
			return exampleDir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loading the example configuration: %v", err)
	}
	return b
}

func TestLoadExample(t *testing.T) {
	b := loadExample(t)

	if got, want := b.Config.CurrentContext, "cluster1"; got != want {
		t.Errorf("currentContext = %q, want %q", got, want)
	}
	if got, want := len(b.Config.Contexts), 2; got != want {
		t.Errorf("contexts = %d, want %d", got, want)
	}
	for _, name := range []string{"cluster1", "cluster2"} {
		if _, ok := b.Clusters[name]; !ok {
			t.Errorf("cluster %q was not indexed", name)
		}
	}
	if _, ok := b.Sites["example"]; !ok {
		t.Error("site example was not indexed")
	}
	if _, ok := b.Inventories["example"]; !ok {
		t.Error("inventory example was not indexed")
	}
}

func TestResolveLayers(t *testing.T) {
	b := loadExample(t)

	r, err := b.Resolve(config.ResolveOptions{Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if got, want := r.SiteName, "example"; got != want {
		t.Errorf("site = %q, want %q", got, want)
	}
	if got, want := r.Spec.Domains["hpc"], "hpc.example.org"; got != want {
		t.Errorf("domains.hpc = %q, want %q", got, want)
	}
	// From the defaults layer, untouched by any file.
	if got, want := r.Spec.BMC.IPMI.Driver, "LAN_2_0"; got != want {
		t.Errorf("bmc.ipmi.driver = %q, want %q", got, want)
	}
	// From the site layer, overriding a default.
	if got, want := r.Spec.BMC.IPMI.Via, "mgmt"; got != want {
		t.Errorf("bmc.ipmi.via = %q, want %q", got, want)
	}
	// From the cluster layer.
	if got, want := r.Spec.Slurm.Organization, "example"; got != want {
		t.Errorf("slurm.organization = %q, want %q", got, want)
	}
	// From the context.
	if got, want := r.Spec.DefaultUser, "alice_adm"; got != want {
		t.Errorf("defaultUser = %q, want %q", got, want)
	}
	if got, want := len(r.Spec.BootPaths), 3; got != want {
		t.Errorf("bootPaths = %d, want %d", got, want)
	}
}

func TestResolveLayerPrecedence(t *testing.T) {
	b := loadExample(t)

	// fanout.max is set by the defaults, the site, and for cluster2 by the
	// context. Each layer must win over the one before it.
	tests := []struct {
		name    string
		context string
		env     map[string]string
		set     map[string]string
		want    int64
		layer   string
	}{
		{name: "site wins over defaults", context: "cluster1", want: 24, layer: v1alpha1.LayerSite},
		{name: "context wins over site", context: "cluster2", want: 6, layer: v1alpha1.LayerContext},
		{
			name: "environment wins over context", context: "cluster2",
			env:  map[string]string{"CLUSTERCTL_FANOUT": "4"},
			want: 4, layer: v1alpha1.LayerEnvironment,
		},
		{
			name: "set wins over everything", context: "cluster2",
			env:  map[string]string{"CLUSTERCTL_FANOUT": "4"},
			set:  map[string]string{"fanout.max": "2"},
			want: 2, layer: v1alpha1.LayerFlags,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := b.Resolve(config.ResolveOptions{
				Context: tc.context,
				Env:     func(k string) string { return tc.env[k] },
				Set:     tc.set,
			})
			if err != nil {
				t.Fatalf("Resolve failed: %v", err)
			}
			if got := int64(r.Spec.Fanout.Max); got != tc.want {
				t.Errorf("fanout.max = %d, want %d", got, tc.want)
			}
			o, ok := r.Tree.Origin("fanout.max")
			if !ok {
				t.Fatal("fanout.max has no recorded origin")
			}
			if o.Layer != tc.layer {
				t.Errorf("fanout.max came from layer %q, want %q", o.Layer, tc.layer)
			}
		})
	}
}

func TestWorkstationLayer(t *testing.T) {
	b := loadExample(t)

	// The workstation lengthens the connection timeout, and the context of
	// cluster2 overrides it again.
	cases := []struct {
		context string
		want    string
		layer   string
	}{
		{"cluster1", "15s", v1alpha1.LayerWorkstation},
		{"cluster2", "20s", v1alpha1.LayerContext},
	}
	for _, tc := range cases {
		t.Run(tc.context, func(t *testing.T) {
			r, err := b.Resolve(config.ResolveOptions{
				Context: tc.context,
				Env:     func(string) string { return "" },
			})
			if err != nil {
				t.Fatalf("Resolve failed: %v", err)
			}
			if got := r.Spec.SSH.ConnectTimeout.String(); got != tc.want {
				t.Errorf("ssh.connectTimeout = %s, want %s", got, tc.want)
			}
			o, _ := r.Tree.Origin("ssh.connectTimeout")
			if o.Layer != tc.layer {
				t.Errorf("layer = %q, want %q", o.Layer, tc.layer)
			}
		})
	}
}

func TestResolveRecordsFilePositions(t *testing.T) {
	b := loadExample(t)
	r, err := b.Resolve(config.ResolveOptions{Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	o, ok := r.Tree.Origin("domains.hpc")
	if !ok {
		t.Fatal("domains.hpc has no recorded origin")
	}
	if o.Layer != v1alpha1.LayerSite {
		t.Errorf("layer = %q, want %q", o.Layer, v1alpha1.LayerSite)
	}
	if filepath.Base(o.File) != "site.yaml" {
		t.Errorf("file = %q, want site.yaml", o.File)
	}
	if o.Line == 0 || o.Column == 0 {
		t.Errorf("origin %v has no position", o)
	}
}

func TestUnknownContextAndCluster(t *testing.T) {
	b := loadExample(t)
	if _, err := b.Resolve(config.ResolveOptions{Context: "nope"}); err == nil {
		t.Error("an unknown context should be reported")
	} else if !strings.Contains(err.Error(), "cluster1") {
		t.Errorf("error = %v, want it to list the known contexts", err)
	}
}

func TestValidationReportsPositionAndSuggestion(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "site.yaml")
	src := `apiVersion: clusterctl/v1alpha1
kind: Site
metadata:
  name: example
spec:
  hosts:
    login:
      host: login.example.org
      forwardAgnet: true
`
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load([]string{file})
	if err == nil {
		t.Fatal("a misspelled field should be rejected")
	}
	msg := err.Error()
	for _, want := range []string{"site.yaml:9:7", "forwardAgnet", "forwardAgent"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestValidationRejectsWrongTypesAndVersions(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "a duration must be a string",
			src: `apiVersion: clusterctl/v1alpha1
kind: Site
spec:
  ssh:
    connectTimeout: 30
`,
			want: "want string",
		},
		{
			name: "an unknown apiVersion is reported",
			src: `apiVersion: clusterctl/v9
kind: Site
spec: {}
`,
			want: "unsupported apiVersion",
		},
		{
			name: "an unknown kind is reported",
			src: `apiVersion: clusterctl/v1alpha1
kind: Nodes
spec: {}
`,
			want: "unknown kind",
		},
		{
			name: "a required field is reported",
			src: `apiVersion: clusterctl/v1alpha1
kind: Cluster
metadata:
  name: c
spec:
  slurm:
    role: login
`,
			want: "site",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "doc.yaml")
			if err := os.WriteFile(file, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load([]string{file})
			if err == nil {
				t.Fatalf("%s should be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLeadingZeroStaysAString(t *testing.T) {
	// A file mode read as a number becomes 384 on some YAML parsers and 600
	// on others, and neither is 0600.
	docs, err := config.ParseDocuments("t.yaml", []byte("mode: 0600\ncount: 42\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := docs[0].Data["mode"], "0600"; got != want {
		t.Errorf("mode = %#v, want the string %q", got, want)
	}
	if got, want := docs[0].Data["count"], int64(42); got != want {
		t.Errorf("count = %#v, want %d", got, want)
	}
}

func TestParseDocumentsRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"a duplicate key", "a: 1\na: 2\n"},
		{"a scalar document", "just a string\n"},
		{"an alias", "a: &x 1\nb: *x\n"},
		{"broken YAML", "a: [1, 2\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.ParseDocuments("t.yaml", []byte(tc.src)); err == nil {
				t.Errorf("%s should be rejected", tc.name)
			}
		})
	}
}

func TestSchemaJSON(t *testing.T) {
	for _, kind := range config.SchemaKinds() {
		data, err := config.SchemaJSON(kind)
		if err != nil {
			t.Fatalf("SchemaJSON(%q) failed: %v", kind, err)
		}
		if !strings.Contains(string(data), "$schema") {
			t.Errorf("the %s schema does not look like JSON Schema", kind)
		}
	}
	if _, err := config.SchemaJSON("Nope"); err == nil {
		t.Error("an unknown kind should be rejected")
	}
}

func TestParseSetValue(t *testing.T) {
	tests := []struct {
		raw  string
		want any
	}{
		{"30s", "30s"},
		{"8", int64(8)},
		{"true", true},
		{"0600", "0600"},
		{"[a, b]", []any{"a", "b"}},
	}
	for _, tc := range tests {
		got, err := config.ParseSetValue(tc.raw)
		if err != nil {
			t.Fatalf("ParseSetValue(%q) failed: %v", tc.raw, err)
		}
		if config.FormatValue(got) != config.FormatValue(tc.want) {
			t.Errorf("ParseSetValue(%q) = %#v, want %#v", tc.raw, got, tc.want)
		}
	}
}
