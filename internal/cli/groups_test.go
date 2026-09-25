// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// fakeSinfo answers the example's slurm group source the way sinfo does: the
// nodes of a partition, nothing for a partition it does not have, and the
// partition names for the listing.
func fakeSinfo(partitions map[string]string) func(transport.Target, transport.Request) (*transport.Result, error) {
	return func(tg transport.Target, req transport.Request) (*transport.Result, error) {
		argv := req.Argv
		if len(argv) == 0 || argv[0] != "sinfo" {
			return &transport.Result{Target: tg}, nil
		}
		switch {
		case slices.Contains(argv, "-p"):
			return &transport.Result{Target: tg, Stdout: partitions[argv[len(argv)-1]] + "\n"}, nil
		case slices.Contains(argv, "-n"):
			node := argv[len(argv)-1]
			var in []string
			for _, name := range slices.Sorted(maps.Keys(partitions)) {
				if strings.Contains(partitions[name], node) {
					in = append(in, name)
				}
			}
			return &transport.Result{Target: tg, Stdout: strings.Join(in, "\n") + "\n"}, nil
		case slices.Contains(argv, "%R"):
			return &transport.Result{Target: tg, Stdout: strings.Join(slices.Sorted(maps.Keys(partitions)), "\n") + "\n"}, nil
		default:
			var all []string
			for _, name := range slices.Sorted(maps.Keys(partitions)) {
				all = append(all, partitions[name])
			}
			return &transport.Result{Target: tg, Stdout: strings.Join(all, ",") + "\n"}, nil
		}
	}
}

// loginDown fails every call to the login node, the way ssh does when the
// host does not answer.
func loginDown(tg transport.Target, _ transport.Request) (*transport.Result, error) {
	if tg.Role == "login" {
		return sshTimedOut(tg), nil
	}
	return &transport.Result{Target: tg}, nil
}

// siteWithCluster2 copies the example configuration into a directory of its
// own, with its second Cluster document replaced by cluster2, and returns
// the directory. A Cluster may be defined only once, so a test that needs
// another cluster2 edits a copy rather than adding a document.
func siteWithCluster2(t *testing.T, cluster2 string) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(exampleDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if e.Name() == "cluster.yaml" {
			docs := strings.Split(string(data), "\n---\n")
			found := false
			for i, doc := range docs {
				if strings.Contains(doc, "\n  name: cluster2\n") {
					docs[i], found = cluster2, true
				}
			}
			if !found {
				t.Fatal("the example has no cluster2 to replace")
			}
			data = []byte(strings.Join(docs, "\n---\n"))
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// cluster2WithSlurm gives the example's second cluster the same cached slurm
// source as the first, which is what two clusters of one site look like.
const cluster2WithSlurm = `apiVersion: clusterctl/v1alpha1
kind: Cluster
metadata:
  name: cluster2
spec:
  site: example
  slurm:
    role: login
    organization: example
  groups:
    defaultSource: inventory
    sources:
      inventory:
        attribute: class
      slurm:
        cacheTtl: 60s
        exec:
          role: login
          map: [sinfo, -h, -o, "%N", -p, $GROUP]
          all: [sinfo, -h, -o, "%N"]
          list: [sinfo, -h, -o, "%R"]
          reverse: [sinfo, -h, -N, -o, "%R", -n, $NODE]
`

// Report 4.2: a cached exec source answered one cluster's lookups from the
// other cluster's cache, with no remote call.
func TestGroupCacheIsKeptPerCluster(t *testing.T) {
	cache := t.TempDir()
	site := siteWithCluster2(t, cluster2WithSlurm)

	first := &transport.Recorder{Reply: fakeSinfo(map[string]string{"main": "exe[0001-0010]"})}
	h, err := run(t, harnessOptions{recorder: first, cacheDir: cache, bare: true, config: []string{site}},
		"--context", "cluster1", "node", "select", "@slurm:main")
	if err != nil {
		t.Fatalf("cluster1 failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "exe[0001-0010]"; got != want {
		t.Fatalf("cluster1 @slurm:main = %q, want %q", got, want)
	}

	second := &transport.Recorder{Reply: fakeSinfo(map[string]string{"main": "sub[0001-0002]"})}
	h, err = run(t, harnessOptions{recorder: second, cacheDir: cache, bare: true, config: []string{site}},
		"--context", "cluster2", "node", "select", "@slurm:main")
	if err != nil {
		t.Fatalf("cluster2 failed: %v", err)
	}
	if len(second.Calls()) == 0 {
		t.Error("cluster2 was answered from cluster1's cache without asking its own source")
	}
	if got, want := strings.TrimSpace(h.out.String()), "sub[0001-0002]"; got != want {
		t.Errorf("cluster2 @slurm:main = %q, want %q", got, want)
	}

	// The cache still serves the cluster that filled it.
	again := &transport.Recorder{Reply: fakeSinfo(map[string]string{"main": "wrong"})}
	h, err = run(t, harnessOptions{recorder: again, cacheDir: cache, bare: true, config: []string{site}},
		"--context", "cluster1", "node", "select", "@slurm:main")
	if err != nil {
		t.Fatalf("cluster1 again failed: %v", err)
	}
	if got := len(again.Calls()); got != 0 {
		t.Errorf("cluster1 asked its source again %d times; the cache should have answered", got)
	}
	if got, want := strings.TrimSpace(h.out.String()), "exe[0001-0010]"; got != want {
		t.Errorf("cluster1 @slurm:main from the cache = %q, want %q", got, want)
	}
}

// Report 4.9: a bare @group fell through to another source when the default
// one could not be asked, and printed that source's nodes with exit 0.
func TestBareGroupDoesNotFallThroughOnAFailure(t *testing.T) {
	answering := &transport.Recorder{Reply: fakeSinfo(map[string]string{
		"main": "exe[0001-0010]", "compute": "exe[0001-0002]",
	})}
	h, err := run(t, harnessOptions{recorder: answering}, "node", "select", "@compute")
	if err != nil {
		t.Fatalf("@compute failed while the login node answers: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "exe[0001-0002]"; got != want {
		t.Fatalf("@compute = %q, want %q", got, want)
	}

	down := &transport.Recorder{Reply: loginDown}
	h, err = run(t, harnessOptions{recorder: down}, "node", "select", "@compute")
	if err == nil {
		t.Fatalf("@compute resolved to %q while the slurm source could not be asked",
			strings.TrimSpace(h.out.String()))
	}
	wantCode(t, err, exitcode.Transport)
	for _, want := range []string{`"slurm"`, "Connection timed out"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// A source that answers that it does not have a group still lets the search
// go on, which is what a bare @group is for.
func TestBareGroupFallsThroughWhenASourceHasNoSuchGroup(t *testing.T) {
	rec := &transport.Recorder{Reply: fakeSinfo(map[string]string{"main": "exe[0001-0010]"})}
	h, err := run(t, harnessOptions{recorder: rec}, "node", "select", "@infra")
	if err != nil {
		t.Fatalf("@infra failed: %v", err)
	}
	if got, want := strings.TrimSpace(h.out.String()), "dbm01,wlm01"; got != want {
		t.Errorf("@infra = %q, want %q", got, want)
	}

	_, err = run(t, harnessOptions{recorder: rec}, "node", "select", "@nosuch")
	if err == nil {
		t.Fatal("a group no source defines resolved")
	}
	wantCode(t, err, exitcode.Usage)
	if !strings.Contains(err.Error(), "no group source defines") {
		t.Errorf("error %q does not say that no source defines the group", err)
	}
}

// Report 11.5: an unreachable group source host exited 2, as if the command
// line were wrong.
func TestUnreachableGroupSourceExitsAsATransportFailure(t *testing.T) {
	for _, args := range [][]string{
		{"exec", "-n", "@slurm:main", "--", "uptime"},
		{"bmc", "power", "off", "-n", "@slurm:main", "--dry-run"},
		{"node", "select", "@slurm:main"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := run(t, harnessOptions{recorder: &transport.Recorder{Reply: loginDown}}, args...)
			if err == nil {
				t.Fatal("the command succeeded without its group source")
			}
			wantCode(t, err, exitcode.Transport)
		})
	}
}

// A group source whose command fails on a host that answered is a failure
// on that host, not a mistake on the command line.
func TestFailingGroupCommandKeepsItsMessage(t *testing.T) {
	rec := &transport.Recorder{Reply: func(tg transport.Target, _ transport.Request) (*transport.Result, error) {
		return &transport.Result{
			Target: tg, ExitCode: 1,
			Stderr: "slurm_load_partitions: Unable to contact slurm controller\n",
			Err:    fmt.Errorf("%s: command exited 1", tg),
		}, nil
	}}
	_, err := run(t, harnessOptions{recorder: rec}, "node", "select", "@slurm:main")
	if err == nil {
		t.Fatal("a failing group command resolved")
	}
	wantCode(t, err, exitcode.TargetFailed)
	if !strings.Contains(err.Error(), "Unable to contact slurm controller") {
		t.Errorf("error %q lost what the command said", err)
	}
}

// Report 4.10: one unreachable exec source hid the memberships every other
// source had found, and node describe exited 0.
func TestNodeGroupsKeepsWhatTheOtherSourcesFound(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"node", "describe", "exe0007"}, []string{"groups.inventory", "groups.rack", "groups.static"}},
		{[]string{"node", "groups", "exe0007"}, []string{"inventory", "rack", "static", "compute"}},
		{[]string{"node", "groups"}, []string{"inventory", "rack", "static", "infra"}},
		{[]string{"node", "groups", "-o", "json"}, []string{`"inventory"`, `"static"`}},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{recorder: &transport.Recorder{Reply: loginDown}}, tc.args...)
			if err == nil {
				t.Fatal("the command succeeded while the slurm source could not be asked")
			}
			wantCode(t, err, exitcode.Transport)
			if !strings.Contains(err.Error(), `"slurm"`) {
				t.Errorf("error %q does not name the failed source", err)
			}
			out := h.out.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output is missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// Report 4.19: the help used @idle and @drained, which no source provides.
// Every group an example names has to resolve against the example site.
func TestHelpExamplesNameGroupsThatExist(t *testing.T) {
	cmd := NewRootCommand(context.Background(), app.Streams{})
	var texts []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		texts = append(texts, c.Long, c.Example)
		texts = append(texts, c.LocalFlags().FlagUsages())
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd)

	refs := map[string]bool{}
	pattern := regexp.MustCompile(`@[A-Za-z0-9_.:-]*[A-Za-z0-9_]`)
	for _, text := range texts {
		for _, ref := range pattern.FindAllString(text, -1) {
			// Redfish's @odata properties are not groups.
			if !strings.HasPrefix(ref, "@odata.") {
				refs[ref] = true
			}
		}
	}
	if len(refs) == 0 {
		t.Fatal("no group reference found in the help; the pattern is wrong")
	}

	// The login node knows the example cluster's partitions and nothing
	// else, so a Slurm state is not mistaken for a partition.
	sinfo := fakeSinfo(map[string]string{"main": "exe[0001-0008]", "debug": "exe[0009-0010]"})
	for _, ref := range slices.Sorted(maps.Keys(refs)) {
		t.Run(ref, func(t *testing.T) {
			_, err := run(t, harnessOptions{recorder: &transport.Recorder{Reply: sinfo}}, "node", "select", ref)
			if err != nil {
				t.Errorf("the help names %s, which does not resolve: %v", ref, err)
			}
		})
	}
}

// Report 4.16: an exec source without a role was documented to run on the
// workstation, which nothing does. The configuration now says so.
func TestExecGroupSourceRequiresARole(t *testing.T) {
	site := siteWithCluster2(t, strings.Replace(cluster2WithSlurm, "          role: login\n", "", 1))
	_, err := run(t, harnessOptions{bare: true, config: []string{site}}, "config", "validate")
	if err == nil {
		t.Fatal("an exec group source without a role was accepted")
	}
	wantCode(t, err, exitcode.Usage)
	if !strings.Contains(err.Error(), "role") || strings.Contains(err.Error(), "second Cluster") {
		t.Errorf("error %q does not name the missing role", err)
	}
}

// node groups looked the name up as typed, so another spelling of the node
// missed the memberships its inventory name has.
func TestNodeGroupsFindsTheMachineOfAnySpelling(t *testing.T) {
	h, err := run(t, harnessOptions{}, "node", "groups", "exe0001", "-o", "json")
	if err != nil {
		t.Fatalf("node groups exe0001 failed: %v", err)
	}
	want := h.out.String()
	if !strings.Contains(want, `"inventory"`) || !strings.Contains(want, `"rack"`) {
		t.Fatalf("node groups exe0001 found no memberships:\n%s", want)
	}

	for _, spelling := range []string{"EXE0001", "exe1", "exe0001.", "exe0001.hpc.example.org"} {
		h, err := run(t, harnessOptions{}, "node", "groups", spelling, "-o", "json")
		if err != nil {
			t.Errorf("node groups %s failed: %v", spelling, err)
			continue
		}
		if got := h.out.String(); got != want {
			t.Errorf("node groups %s = %s, want what exe0001 has: %s", spelling, got, want)
		}
	}
}

// NODE is one node, and a name that is not a host name is refused before
// any source is asked about it.
func TestNodeGroupsRefusesWhatIsNotOneNode(t *testing.T) {
	for _, arg := range []string{"exe[0001-0002]", "-exe0001", "exe0001;id", " "} {
		h, err := run(t, harnessOptions{}, "node", "groups", "--", arg)
		if err == nil {
			t.Errorf("node groups %q: accepted, output:\n%s", arg, h.out)
			continue
		}
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("node groups %q: exit code = %d, want %d (%v)", arg, got, exitcode.Usage, err)
		}
		if calls := h.recorder.Calls(); len(calls) != 0 {
			t.Errorf("node groups %q: sent %d commands, want none", arg, len(calls))
		}
	}
}
