// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GSI-HPC/clusterctl/nodeset"
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

func (r *batchResolver) ResolveAll(refs []nodeset.GroupRef) []nodeset.GroupAnswer {
	names := make([]string, len(refs))
	answers := make([]nodeset.GroupAnswer, len(refs))
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
			"rack": {"d": "n4", "r1": "n[1-4]"},
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
	ns, err := nodeset.ParseWith("@a,@b!n1 @rack:r1&@e", res)
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

// An expression that names one group has nothing to look up side by side,
// and asks for it as before.
func TestASingleGroupIsResolvedAsBefore(t *testing.T) {
	t.Parallel()
	res := newBatchResolver()
	if _, err := nodeset.ParseWith("@a,n7", res); err != nil {
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
	_, err := nodeset.ParseWith("@a,@gone", res)
	if err == nil || !strings.Contains(err.Error(), "group @gone: the source did not answer") {
		t.Fatalf("error = %v, want the lookup's failure", err)
	}
	if want := []string{"all :a :gone"}; !slices.Equal(res.asked, want) {
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
		"@a,@a,@a", "x[1-2]&@rack:r1,@c", "@a,@b:c,@:a",
	} {
		plain, plainErr := nodeset.ParseWith(expr, onlyResolver{newBatchResolver()})
		batched, batchedErr := nodeset.ParseWith(expr, newBatchResolver())
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
