// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeexpr_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/go-nodeset"

	"github.com/GSI-HPC/clusterctl/internal/nodeexpr"
)

// batchResolver is a MapResolver that can look up several groups at once,
// and records how it was asked: each ResolveAll as the references it was
// given, and each Resolve as the one reference.
type batchResolver struct {
	*nodeset.MapResolver
	// failing are groups whose lookup fails.
	failing map[string]bool

	mu    sync.Mutex
	asked []string
}

func (r *batchResolver) record(s string) {
	r.mu.Lock()
	r.asked = append(r.asked, s)
	r.mu.Unlock()
}

func (r *batchResolver) lookup(source, group string) (string, error) {
	if r.failing[group] {
		return "", errors.New("the source did not answer")
	}
	return r.MapResolver.Resolve(source, group)
}

func (r *batchResolver) Resolve(source, group string) (string, error) {
	r.record("resolve " + source + ":" + group)
	return r.lookup(source, group)
}

func (r *batchResolver) ResolveAll(refs []nodeexpr.GroupRef) []nodeexpr.GroupAnswer {
	names := make([]string, len(refs))
	answers := make([]nodeexpr.GroupAnswer, len(refs))
	for i, ref := range refs {
		names[i] = ref.Source + ":" + ref.Group
		answers[i].Expr, answers[i].Err = r.lookup(ref.Source, ref.Group)
	}
	r.record("all " + strings.Join(names, " "))
	return answers
}

func newBatchResolver() *batchResolver {
	return &batchResolver{
		MapResolver: &nodeset.MapResolver{Default: "site", Groups: map[string]map[string]string{
			"site": {"a": "n[1-2]", "b": "@c,@rack:d", "c": "n3", "e": "n[1-5]!@a", "lost": "@gone,n9", "gone": "n8"},
			"rack": {"d": "n4", "r1": "n[1-4]", "r2": "@d,@r1", "r3": "@:d,@site:c"},
		}},
		failing: map[string]bool{"gone": true},
	}
}

// An expression's group references were looked up one after the other,
// each a round trip when the source runs a command on a host. A resolver
// that can look up several at once is given the groups of each level of
// the expression together, and the evaluation takes their answers without
// asking again.
func TestTheGroupsOfALevelAreLookedUpTogether(t *testing.T) {
	t.Parallel()
	res := newBatchResolver()
	ns, err := nodeexpr.ParseWith("@a,@b!n1 @rack:r1&@e", res)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ns.String(), "n[3-4]"; got != want {
		t.Errorf("set = %s, want %s", got, want)
	}
	want := []string{"all :a :b rack:r1 :e", "all :c rack:d"}
	if !slices.Equal(res.asked, want) {
		t.Errorf("the resolver was asked\n%s\nwant\n%s", strings.Join(res.asked, "\n"), strings.Join(want, "\n"))
	}
}

// go-nodeset resolves a bare reference inside a group of a named source in
// that source, so that is where the groups of that level are looked up.
func TestTheGroupsOfAGroupOfANamedSourceAreLookedUpInIt(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]struct {
		set   string
		asked []string
	}{
		"@rack:r2": {"n[1-4]", []string{"resolve rack:r2", "all rack:d rack:r1"}},
		"@rack:r3": {"n[3-4]", []string{"resolve rack:r3", "all rack:d site:c"}},
	} {
		res := newBatchResolver()
		ns, err := nodeexpr.ParseWith(expr, res)
		if err != nil {
			t.Fatal(err)
		}
		if got := ns.String(); got != want.set {
			t.Errorf("%s = %s, want %s", expr, got, want.set)
		}
		if !slices.Equal(res.asked, want.asked) {
			t.Errorf("%s: the resolver was asked %q, want %q", expr, res.asked, want.asked)
		}
	}
}

// The groups that a source's every group, @source:*, names are looked up
// together, in that source, as those of a group are.
func TestTheGroupsOfAllOfASourceAreLookedUpTogether(t *testing.T) {
	t.Parallel()
	res := newBatchResolver()
	ns, err := nodeexpr.ParseWith("@rack:*", res)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ns.String(), "n[1-4]"; got != want {
		t.Errorf("set = %s, want %s", got, want)
	}
	if want := []string{"all rack:d rack:r1 site:c"}; !slices.Equal(res.asked, want) {
		t.Errorf("the resolver was asked %q, want %q", res.asked, want)
	}
}

// An expression that names one group has nothing to look up side by side,
// and asks for it as before.
func TestASingleGroupIsResolvedAsBefore(t *testing.T) {
	t.Parallel()
	res := newBatchResolver()
	if _, err := nodeexpr.ParseWith("@a,n7", res); err != nil {
		t.Fatal(err)
	}
	if want := []string{"resolve :a"}; !slices.Equal(res.asked, want) {
		t.Errorf("the resolver was asked %q, want %q", res.asked, want)
	}
}

// A lookup that failed is not made again by the evaluation, which reports
// that failure: a resolver does not remember a failure that may not last,
// and a second lookup could even answer otherwise.
func TestAFailedLookupIsTheEvaluationsAnswer(t *testing.T) {
	t.Parallel()
	res := newBatchResolver()
	_, err := nodeexpr.ParseWith("@a,@gone", res)
	if err == nil || !strings.Contains(err.Error(), "group @gone: the source did not answer") {
		t.Fatalf("error = %v, want the lookup's failure", err)
	}
	if want := []string{"all :a :gone"}; !slices.Equal(res.asked, want) {
		t.Errorf("the resolver was asked %q, want %q", res.asked, want)
	}
}

// One Batch kept across expressions looks each group up once, the
// protected hosts entries' groups together.
func TestABatchKeptAcrossExpressionsLooksEachGroupUpOnce(t *testing.T) {
	t.Parallel()
	res := newBatchResolver()
	b := nodeexpr.NewBatch(res)
	entries := []string{"@a", "@c", "@gone"}
	b.Prefetch(entries...)
	for _, expr := range entries {
		_, _ = nodeexpr.ParseWith(expr, b)
	}
	if want := []string{"all :a :c :gone"}; !slices.Equal(res.asked, want) {
		t.Errorf("the resolver was asked %q, want %q", res.asked, want)
	}
}

// Looked up together or one after the other, an expression names the same
// hosts, or fails with the same error, the first the evaluation meets.
func TestBatchedGroupsEvaluateAsResolvedOnes(t *testing.T) {
	t.Parallel()
	for _, expr := range []string{
		"@a", "@a,@b", "@b!@a", "@e&@rack:r1", "@rack:*", "@*", "@a,@lost", "@lost,@a",
		"@gone,@nope", "@nope,@gone", "@a@b", "@", "@site:", "n[1-3],@a ^ @rack:d", "@a,[", "@b]",
		"@a,@a,@a", "x[1-2]&@rack:r1,@c", "@a,@b:c,@:a", "@rack:r2,@rack:r3",
	} {
		plain, plainErr := nodeexpr.ParseWith(expr, onlyResolver{newBatchResolver()})
		batched, batchedErr := nodeexpr.ParseWith(expr, newBatchResolver())
		if fmt.Sprint(plainErr) != fmt.Sprint(batchedErr) {
			t.Errorf("%q: error %v, want %v", expr, batchedErr, plainErr)
			continue
		}
		if plainErr == nil && plain.String() != batched.String() {
			t.Errorf("%q = %s, want %s", expr, batched, plain)
		}
	}
}

// onlyResolver hides that a resolver can look up several groups at once.
type onlyResolver struct{ nodeset.Resolver }

// groupRefs reads references the way go-nodeset reads them.
func TestGroupRefsReadsReferencesAsGoNodesetDoes(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]string{
		"@a,@b!n1 @rack:r1&@e":   ":a :b rack:r1 :e",
		"@a[1,2],@b":             ":a[1,2] :b",
		" @a\t@b\n@c\r@d^@e":     ":a :b :c :d :e",
		"@*,@rack:*,@,@rack:,@x": ":x",
		"@:a,@s:g:h":             ":a s:g:h",
		"x@a,@b":                 ":b",
	} {
		var got []string
		for _, ref := range nodeexpr.GroupRefs(expr) {
			got = append(got, ref.Source+":"+ref.Group)
		}
		if strings.Join(got, " ") != want {
			t.Errorf("groupRefs(%q) = %q, want %q", expr, strings.Join(got, " "), want)
		}
	}
}
