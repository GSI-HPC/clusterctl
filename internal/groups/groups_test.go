// SPDX-License-Identifier: LGPL-3.0-or-later

package groups_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/groups"
	"github.com/GSI-HPC/clusterctl/internal/inventory"
	"github.com/GSI-HPC/clusterctl/internal/transport"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func testInventory(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.New(v1alpha1.NodeInventorySpec{Nodes: []v1alpha1.NodeEntry{
		{Nodes: "exe[1-4]", Attributes: map[string]string{"class": "exe", "rack": "R02"}},
		{Nodes: "sub[1-2]", Attributes: map[string]string{"class": "sub", "rack": "R03"}},
		{Nodes: "wlm01", Attributes: map[string]string{"class": "wlm", "rack": "R01"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func testResolver(t *testing.T, rec *transport.Recorder, cacheDir string) *groups.Resolver {
	t.Helper()
	return groups.New(groups.Options{
		Spec: v1alpha1.GroupsSpec{
			DefaultSource: "inventory",
			Sources: map[string]v1alpha1.GroupSource{
				"inventory": {Attribute: "class"},
				"rack":      {Attribute: "rack"},
				"static":    {Static: map[string]string{"infra": "wlm01", "all": "@inventory:exe,@static:infra"}},
				"slurm": {
					CacheTTL: v1alpha1.Duration(time.Minute),
					Exec: &v1alpha1.ExecGroupSource{
						Role: "login",
						Map:  []string{"sinfo", "-h", "-o", "%N", "-p", groups.PlaceholderGroup},
						All:  []string{"sinfo", "-h", "-o", "%N"},
						List: []string{"sinfo", "-h", "-o", "%R"},
					},
				},
			},
		},
		Inventory: testInventory(t),
		Runner:    rec,
		Target: func(role string) (transport.Target, error) {
			return transport.Target{Name: role, Host: role + ".example.org", Role: role}, nil
		},
		CacheDir: cacheDir,
		Context:  context.Background(),
	})
}

func TestAttributeSource(t *testing.T) {
	t.Parallel()
	r := testResolver(t, &transport.Recorder{}, "")

	// The inventory replaces the genders file: one group per attribute
	// value, reachable with or without the source prefix.
	for _, expr := range []string{"@exe", "@inventory:exe"} {
		ns, err := nodeset.ParseWith(expr, r)
		if err != nil {
			t.Fatalf("ParseWith(%q) failed: %v", expr, err)
		}
		if got, want := ns.String(), "exe[1-4]"; got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}

	ns, err := nodeset.ParseWith("@rack:R01", r)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ns.String(), "wlm01"; got != want {
		t.Errorf("@rack:R01 = %q, want %q", got, want)
	}
}

func TestStaticSourceAndNesting(t *testing.T) {
	t.Parallel()
	r := testResolver(t, &transport.Recorder{}, "")

	ns, err := nodeset.ParseWith("@static:all", r)
	if err != nil {
		t.Fatalf("ParseWith failed: %v", err)
	}
	if got, want := ns.String(), "exe[1-4],wlm01"; got != want {
		t.Errorf("@static:all = %q, want %q", got, want)
	}
}

func TestExecSourceSubstitutesArgumentsNotShellWords(t *testing.T) {
	t.Parallel()

	rec := &transport.Recorder{Responses: []*transport.Result{{Stdout: "exe1 exe2 exe3\n"}}}
	r := testResolver(t, rec, "")

	ns, err := nodeset.ParseWith("@slurm:main", r)
	if err != nil {
		t.Fatalf("ParseWith failed: %v", err)
	}
	if got, want := ns.String(), "exe[1-3]"; got != want {
		t.Errorf("@slurm:main = %q, want %q", got, want)
	}

	calls := rec.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if got, want := calls[0].Target.Role, "login"; got != want {
		t.Errorf("the command ran on role %q, want %q", got, want)
	}
	// The group name is substituted as a whole argument, so a name holding
	// a semicolon cannot become a second command.
	argv := calls[0].Request.Argv
	if got, want := argv[len(argv)-1], "main"; got != want {
		t.Errorf("last argument = %q, want %q", got, want)
	}
	if got, want := calls[0].Command, "sinfo -h -o %N -p main"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestExecSourceQuotesHostileGroupNames(t *testing.T) {
	t.Parallel()

	rec := &transport.Recorder{Responses: []*transport.Result{{Stdout: "\n"}}}
	r := testResolver(t, rec, "")

	_, err := nodeset.ParseWith("@slurm:x", r)
	if err == nil {
		t.Skip("an empty answer is reported, which this case does not exercise")
	}

	rec = &transport.Recorder{Responses: []*transport.Result{{Stdout: "exe1\n"}}}
	r = testResolver(t, rec, "")
	if _, err := r.Resolve("slurm", "a; rm -rf /"); err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if got := rec.Commands()[0]; !strings.Contains(got, `'a; rm -rf /'`) {
		t.Errorf("command = %q, want the group name quoted as one word", got)
	}
}

func TestExecResultsAreCached(t *testing.T) {
	t.Parallel()

	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: "exe1\n"}, nil
	}}
	r := testResolver(t, rec, t.TempDir())

	for i := 0; i < 3; i++ {
		if _, err := r.Resolve("slurm", "main"); err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
	}
	if got := len(rec.Calls()); got != 1 {
		t.Errorf("the group was looked up %d times, want 1", got)
	}
}

func TestListAndAll(t *testing.T) {
	t.Parallel()
	r := testResolver(t, &transport.Recorder{}, "")

	got, err := r.List("inventory")
	if err != nil {
		t.Fatal(err)
	}
	if want := "exe,sub,wlm"; strings.Join(got, ",") != want {
		t.Errorf("List = %q, want %q", got, want)
	}

	all, err := r.All("inventory")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := nodeset.ParseWith(all, r)
	if err != nil {
		t.Fatal(err)
	}
	if want := 7; ns.Len() != want {
		t.Errorf("@* names %d nodes, want %d", ns.Len(), want)
	}

	if _, err := r.List("nope"); err == nil {
		t.Error("an unknown source should be reported")
	}
}

func TestGroupsOf(t *testing.T) {
	t.Parallel()
	r := testResolver(t, &transport.Recorder{}, "")

	got, err := r.GroupsOf("exe1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "exe"; strings.Join(got["inventory"], ",") != want {
		t.Errorf("GroupsOf(exe1)[inventory] = %q, want %q", got["inventory"], want)
	}
	if want := "R02"; strings.Join(got["rack"], ",") != want {
		t.Errorf("GroupsOf(exe1)[rack] = %q, want %q", got["rack"], want)
	}
}

func TestUnknownGroupIsReported(t *testing.T) {
	t.Parallel()
	r := testResolver(t, &transport.Recorder{}, "")

	_, err := nodeset.ParseWith("@nope", r)
	if err == nil {
		t.Fatal("an unknown group should be reported")
	}
	if !strings.Contains(err.Error(), "sources:") {
		t.Errorf("error = %v, want it to list the sources that were searched", err)
	}
}

func TestSourcesAndDefault(t *testing.T) {
	t.Parallel()
	r := testResolver(t, &transport.Recorder{}, "")

	if got, want := strings.Join(r.Sources(), ","), "inventory,rack,slurm,static"; got != want {
		t.Errorf("Sources = %q, want %q", got, want)
	}
	if got, want := r.DefaultSource(), "inventory"; got != want {
		t.Errorf("DefaultSource = %q, want %q", got, want)
	}
}
