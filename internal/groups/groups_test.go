// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package groups_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
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
	return groups.New(testOptions(t, rec, cacheDir))
}

func testOptions(t *testing.T, rec *transport.Recorder, cacheDir string) groups.Options {
	t.Helper()
	return groups.Options{
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
	}
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

	for range 3 {
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

// answer replies to every command with the same nodes.
func answer(nodes string) *transport.Recorder {
	return &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, Stdout: nodes + "\n"}, nil
	}}
}

// unreachable fails every command the way ssh does when the host does not
// answer.
func unreachable() *transport.Recorder {
	return &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{Target: tg, ExitCode: 255,
			Err: exitcode.Wrap(exitcode.Transport, fmt.Errorf("%s: Connection timed out", tg))}, nil
	}}
}

// Report 4.2: the on-disk cache is shared by every configuration of a user,
// so an entry may only answer the scope, host and command that wrote it.
func TestDiskCacheIsKeptPerScopeHostAndGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	fill := testOptions(t, answer("exe1"), dir)
	fill.Scope = "cluster1"
	if _, err := groups.New(fill).Resolve("slurm", "gpu/a100"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		change func(*groups.Options)
		group  string
		cached bool
	}{
		{"the same lookup", func(*groups.Options) {}, "gpu/a100", true},
		{"another cluster", func(o *groups.Options) { o.Scope = "cluster2" }, "gpu/a100", false},
		{"a name that only differs in a slash", func(*groups.Options) {}, "gpu_a100", false},
		{"another host", func(o *groups.Options) {
			o.Target = func(role string) (transport.Target, error) {
				return transport.Target{Name: role, Host: "login.other.org", Role: role}, nil
			}
		}, "gpu/a100", false},
		{"another command", func(o *groups.Options) {
			src := o.Spec.Sources["slurm"]
			exec := *src.Exec
			exec.Map = []string{"sinfo", "-h", "-o", "%N", "-M", "other", "-p", groups.PlaceholderGroup}
			src.Exec = &exec
			sources := map[string]v1alpha1.GroupSource{}
			maps.Copy(sources, o.Spec.Sources)
			sources["slurm"] = src
			o.Spec.Sources = sources
		}, "gpu/a100", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := answer("exe2")
			opts := testOptions(t, rec, dir)
			opts.Scope = "cluster1"
			tc.change(&opts)
			got, err := groups.New(opts).Resolve("slurm", tc.group)
			if err != nil {
				t.Fatal(err)
			}
			want := "exe2"
			if tc.cached {
				want = "exe1"
			}
			if got != want {
				t.Errorf("Resolve = %q, want %q", got, want)
			}
			if asked := len(rec.Calls()) > 0; asked == tc.cached {
				t.Errorf("asked the source: %v, want %v", asked, !tc.cached)
			}
		})
	}
}

// Report 4.16: the commands of an exec source had no timeout.
func TestExecSourceBoundsItsCommands(t *testing.T) {
	t.Parallel()
	rec := answer("exe1")
	opts := testOptions(t, rec, "")
	opts.Timeout = 30 * time.Second
	if _, err := groups.New(opts).Resolve("slurm", "main"); err != nil {
		t.Fatal(err)
	}
	call := rec.Calls()[0]
	if got, want := call.Request.Timeout, 30*time.Second; got != want {
		t.Errorf("timeout = %v, want %v", got, want)
	}
	if !strings.HasPrefix(call.Command, "timeout -k") {
		t.Errorf("command = %q, want the timeout enforced on the host", call.Command)
	}
	if got, want := call.Request.TTY, transport.TTYNone; got != want {
		t.Errorf("TTY = %v, want %v", got, want)
	}
}

// Report 4.16: an exec source without a role was documented to run locally
// but failed with a message that did not say why.
func TestExecSourceNeedsARole(t *testing.T) {
	t.Parallel()
	rec := answer("exe1")
	opts := testOptions(t, rec, "")
	opts.Spec.Sources = map[string]v1alpha1.GroupSource{
		"slurm": {Exec: &v1alpha1.ExecGroupSource{Map: []string{"sinfo", "-p", groups.PlaceholderGroup}}},
	}
	_, err := groups.New(opts).Resolve("slurm", "main")
	if err == nil {
		t.Fatal("an exec source without a role resolved")
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(err.Error(), "exec.role") {
		t.Errorf("error %q does not say what to set", err)
	}
	if len(rec.Calls()) != 0 {
		t.Error("a command was sent without a role")
	}
}

// Report 4.9: a bare @group moves on only when a source answers that it has
// no such group.
func TestBareGroupSearch(t *testing.T) {
	t.Parallel()
	sinfo := func(partitions string, nodes string) *transport.Recorder {
		return &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
			if strings.Contains(strings.Join(req.Argv, " "), "%R") {
				return &transport.Result{Target: tg, Stdout: partitions}, nil
			}
			return &transport.Result{Target: tg, Stdout: nodes}, nil
		}}
	}
	tests := []struct {
		name     string
		rec      *transport.Recorder
		group    string
		want     string
		notFound bool
		code     int
	}{
		{"a source that lists no such group is passed", sinfo("main\n", ""), "infra", "wlm01", false, 0},
		{"a source that has it answers", sinfo("infra\n", "exe1\n"), "infra", "exe1", false, 0},
		{"a group a source lists but returns empty for stops the search", sinfo("infra\n", ""), "infra", "", false, exitcode.Usage},
		{"an unreachable source stops the search", unreachable(), "infra", "", false, exitcode.Transport},
		{"no source defines it", sinfo("main\n", ""), "nope", "", true, exitcode.Usage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := testResolver(t, tc.rec, "").Resolve("", tc.group)
			if tc.want != "" {
				if err != nil || got != tc.want {
					t.Fatalf("Resolve = %q, %v, want %q", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("Resolve = %q, want an error", got)
			}
			if errors.Is(err, groups.ErrNotDefined) != tc.notFound {
				t.Errorf("errors.Is(%v, ErrNotDefined) = %v, want %v", err, !tc.notFound, tc.notFound)
			}
			if tc.code == exitcode.Transport && exitcode.From(err) != tc.code {
				t.Errorf("exit code = %d, want %d", exitcode.From(err), tc.code)
			}
			if !tc.notFound && !strings.Contains(err.Error(), `"slurm"`) {
				t.Errorf("error %q does not name the source that stopped the search", err)
			}
		})
	}
}

// Report 4.10: one failing source threw away what every other source found.
func TestGroupsOfKeepsWhatTheOtherSourcesFound(t *testing.T) {
	t.Parallel()
	opts := testOptions(t, unreachable(), "")
	slurm := opts.Spec.Sources["slurm"]
	exec := *slurm.Exec
	exec.Reverse = []string{"sinfo", "-h", "-N", "-o", "%R", "-n", groups.PlaceholderNode}
	slurm.Exec = &exec
	opts.Spec.Sources["slurm"] = slurm

	got, err := groups.New(opts).GroupsOf("wlm01")
	if err == nil {
		t.Fatal("a failing source was not reported")
	}
	if got, want := exitcode.From(err), exitcode.Transport; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	for source, want := range map[string]string{"inventory": "wlm", "rack": "R01", "static": "all,infra"} {
		if strings.Join(got[source], ",") != want {
			t.Errorf("GroupsOf(wlm01)[%s] = %q, want %q", source, got[source], want)
		}
	}
}

// List ran the command of an exec source on every call, where Map and All
// were cached; an exec source's groups now come from the same caches.
func TestExecListIsCached(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	rec := answer("main,gpu")
	r := testResolver(t, rec, dir)
	for range 3 {
		got, err := r.List("slurm")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != "main,gpu" {
			t.Fatalf("List = %q, want main,gpu", got)
		}
	}
	if got := len(rec.Calls()); got != 1 {
		t.Errorf("the groups were listed %d times, want 1", got)
	}

	// Another process within the TTL reads the list from disk.
	again := answer("other")
	got, err := testResolver(t, again, dir).List("slurm")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "main,gpu" || len(again.Calls()) != 0 {
		t.Errorf("List = %q after %d commands, want main,gpu from the cache", got, len(again.Calls()))
	}
}

// Whether a group is not a source's is decided on a fresh listing: a cached
// one that predates the group would pass the search on to another source's
// group of the same name.
func TestAStaleListDoesNotPassTheSearchOn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := testResolver(t, answer("main"), dir).List("slurm"); err != nil {
		t.Fatal(err)
	}

	rec := &transport.Recorder{Reply: func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		if strings.Contains(strings.Join(req.Argv, " "), "%R") {
			return &transport.Result{Target: tg, Stdout: "main\ninfra\n"}, nil
		}
		return &transport.Result{Target: tg}, nil
	}}
	got, err := testResolver(t, rec, dir).Resolve("", "infra")
	if err == nil {
		t.Fatalf("Resolve = %q, want the slurm source to stop the search", got)
	}
	if errors.Is(err, groups.ErrNotDefined) {
		t.Errorf("error %v says the group is not defined, but the source lists it", err)
	}
}
