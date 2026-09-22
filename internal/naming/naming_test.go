// SPDX-License-Identifier: LGPL-3.0-or-later

package naming_test

import (
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/naming"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func exampleNamer(t *testing.T) *naming.Namer {
	t.Helper()
	spec := v1alpha1.NamingSpec{
		Rules: []v1alpha1.NamingRule{
			{
				Match: v1alpha1.NamingMatch{Prefixes: []string{"exe", "sub", "wlm", "dbm"}},
				FQDN:  "{name}.{domains.hpc}",
				BMC:   "{name}.{domains.mgmtHpc}",
			},
			{
				Match: v1alpha1.NamingMatch{Pattern: `^srv\d+$`},
				FQDN:  "{name}.{domains.infra}",
				BMC:   "{bmcPrefix}{name}.{domains.mgmt}",
			},
			{
				FQDN: "{name}.{domains.site}",
				BMC:  "{bmcPrefix}{name}.{domains.mgmt}",
			},
		},
		BMCPrefix: "bmc-",
	}
	n, err := naming.New(spec, map[string]string{
		"hpc":     "hpc.example.org",
		"site":    "example.org",
		"mgmt":    "mgmt.example.org",
		"mgmtHpc": "mgmt.hpc.example.org",
		"infra":   "infra.example.org",
	})
	if err != nil {
		t.Fatalf("compiling the naming rules: %v", err)
	}
	return n
}

func TestFQDN(t *testing.T) {
	t.Parallel()
	n := exampleNamer(t)

	tests := []struct{ node, want string }{
		{"exe0001", "exe0001.hpc.example.org"},
		{"wlm01", "wlm01.hpc.example.org"},
		{"srv7", "srv7.infra.example.org"},
		{"other", "other.example.org"},
		// A name that already carries a domain is left exactly as it is.
		{"exe0001.other.org", "exe0001.other.org"},
	}
	for _, tc := range tests {
		got, err := n.FQDN(tc.node)
		if err != nil {
			t.Errorf("FQDN(%q) failed: %v", tc.node, err)
			continue
		}
		if got != tc.want {
			t.Errorf("FQDN(%q) = %q, want %q", tc.node, got, tc.want)
		}
	}
}

func TestBMC(t *testing.T) {
	t.Parallel()
	n := exampleNamer(t)

	tests := []struct{ node, want string }{
		{"exe0001", "exe0001.mgmt.hpc.example.org"},
		{"other", "bmc-other.mgmt.example.org"},
		// The node's own domain is dropped: the service processor lives in
		// another one.
		{"exe0001.hpc.example.org", "exe0001.mgmt.hpc.example.org"},
	}
	for _, tc := range tests {
		got, err := n.BMC(tc.node)
		if err != nil {
			t.Errorf("BMC(%q) failed: %v", tc.node, err)
			continue
		}
		if got != tc.want {
			t.Errorf("BMC(%q) = %q, want %q", tc.node, got, tc.want)
		}
	}
}

func TestSetsFold(t *testing.T) {
	t.Parallel()
	n := exampleNamer(t)

	hosts, err := n.FQDNSet(nodeset.MustParse("exe[1-3]"))
	if err != nil {
		t.Fatalf("FQDNSet failed: %v", err)
	}
	if got, want := hosts.String(), "exe[1-3].hpc.example.org"; got != want {
		t.Errorf("FQDNSet = %q, want %q", got, want)
	}

	bmcs, err := n.BMCSet(nodeset.MustParse("exe[1-3]"))
	if err != nil {
		t.Fatalf("BMCSet failed: %v", err)
	}
	if got, want := bmcs.String(), "exe[1-3].mgmt.hpc.example.org"; got != want {
		t.Errorf("BMCSet = %q, want %q", got, want)
	}
}

func TestMissingDomainIsReported(t *testing.T) {
	t.Parallel()

	n, err := naming.New(v1alpha1.NamingSpec{
		Rules: []v1alpha1.NamingRule{{FQDN: "{name}.{domains.hpc}"}},
	}, map[string]string{"hpc": ""})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	// An unset domain would otherwise produce "exe1." and fail much later,
	// in a DNS lookup that says nothing about the configuration.
	if _, err := n.FQDN("exe1"); err == nil {
		t.Error("naming with an unset domain should be reported")
	}
}

func TestNewRejectsBadRules(t *testing.T) {
	t.Parallel()

	cases := map[string]v1alpha1.NamingSpec{
		"no rules at all": {},
		"an invalid pattern": {Rules: []v1alpha1.NamingRule{
			{Match: v1alpha1.NamingMatch{Pattern: "("}, FQDN: "{name}"},
		}},
		"a rule that names nothing": {Rules: []v1alpha1.NamingRule{
			{Match: v1alpha1.NamingMatch{Prefixes: []string{"exe"}}},
		}},
	}
	for name, spec := range cases {
		if _, err := naming.New(spec, nil); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestUnknownPlaceholder(t *testing.T) {
	t.Parallel()

	n, err := naming.New(v1alpha1.NamingSpec{
		Rules: []v1alpha1.NamingRule{{FQDN: "{name}.{domains.nope}"}},
	}, map[string]string{"hpc": "hpc.example.org"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if _, err := n.FQDN("exe1"); err == nil {
		t.Error("an unknown placeholder should be reported")
	}
}

func TestShort(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"exe1.hpc.example.org": "exe1",
		"exe1":                 "exe1",
	} {
		if got := naming.Short(in); got != want {
			t.Errorf("Short(%q) = %q, want %q", in, got, want)
		}
	}
}
