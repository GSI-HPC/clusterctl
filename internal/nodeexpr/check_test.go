// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeexpr_test

import (
	"testing"

	"github.com/GSI-HPC/go-nodeset"

	"github.com/GSI-HPC/clusterctl/internal/nodeexpr"
)

// A range without its last bound is what exe[1-$N] leaves behind when N is
// empty. go-nodeset reads exe[1-] as exe1 and exe[1-,5] as exe[1,5], which
// would select one host where the command meant several, so each is
// refused, as clusterctl's own parser refused it.
func TestARangeWithoutItsLastBoundIsRefused(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]string{
		"exe[1-]":             `in "exe[1-]": the range "1-" has no last bound`,
		"exe[1-,5]":           `in "exe[1-,5]": the range "1-" has no last bound`,
		"exe[1-/2]":           `in "exe[1-/2]": the range "1-" has no last bound`,
		"exe[5,1-]":           `in "exe[5,1-]": the range "1-" has no last bound`,
		"exe[ 1- ,5]":         `in "exe[ 1- ,5]": the range "1-" has no last bound`,
		"exe[0001-]":          `in "exe[0001-]": the range "0001-" has no last bound`,
		"exe[1-2]-ib[0-]":     `in "exe[1-2]-ib[0-]": the range "0-" has no last bound`,
		"rack[1-]node[1-2]":   `in "rack[1-]node[1-2]": the range "1-" has no last bound`,
		"sub1,exe[1-]!exe2":   `in "exe[1-]": the range "1-" has no last bound`,
		"exe[1-3]&exe[1-]":    `in "exe[1-]": the range "1-" has no last bound`,
		"sub1 exe[1-] sub2":   `in "exe[1-]": the range "1-" has no last bound`,
		"exe[1-].example.org": `in "exe[1-].example.org": the range "1-" has no last bound`,
	} {
		for name, parse := range map[string]func(string) (*nodeset.NodeSet, error){
			"Parse": nodeexpr.Parse,
			"ParseWith": func(expr string) (*nodeset.NodeSet, error) {
				return nodeexpr.ParseWith(expr, nodeset.NewMapResolver("local", nil))
			},
			"Add": func(expr string) (*nodeset.NodeSet, error) {
				ns := nodeset.New()
				return ns, nodeexpr.Add(ns, expr)
			},
		} {
			ns, err := parse(expr)
			if err == nil {
				t.Errorf("%s(%q) = %s, want the range refused", name, expr, ns)
				continue
			}
			if err.Error() != want {
				t.Errorf("%s(%q): error %q, want %q", name, expr, err, want)
			}
		}
	}
}

// What has every bound it needs still parses, a dash outside the brackets
// included, and what go-nodeset refuses for another reason is refused with
// its own error.
func TestWhatHasItsBoundsParsesAsBefore(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]string{
		"node-1":               "node-1",
		"a-[1-2]":              "a-[1-2]",
		"exe[1-2]-x":           "exe[1-2]-x",
		"exe[1-2]-ib[0-1]":     "exe[1-2]-ib[0-1]",
		"worker-[0-2]":         "worker-[0-2]",
		"exe[1-10/3]":          "exe[1,4,7,10]",
		"exe[1,5,9]":           "exe[1,5,9]",
		"exe[ 1-3 , 5 ]":       "exe[1-3,5]",
		"exe[0001-0003]":       "exe[0001-0003]",
		"exe[1-5]!exe[2-3]":    "exe[1,4-5]",
		"10.0.1.[1-4]":         "10.0.1.[1-4]",
		"exe[1-3],":            "exe[1-3]",
		"exe[1-2].example.org": "exe[1-2].example.org",
	} {
		ns, err := nodeexpr.Parse(expr)
		if err != nil {
			t.Errorf("Parse(%q) failed: %v", expr, err)
			continue
		}
		if got := ns.String(); got != want {
			t.Errorf("Parse(%q) = %s, want %s", expr, got, want)
		}
	}
	for _, expr := range []string{
		"exe[1-", "exe[-1]", "exe[1--2]", "exe[a-]", "exe[1-/0]", "exe[1-/x]",
		"exe[1-2-3]", "exe[,1-]", "-exe[1-]", "exe0[1-]", "exe[1-2][1-]", "exe]1-[", "exe[5-1]",
	} {
		_, got := nodeexpr.Parse(expr)
		_, want := nodeset.Parse(expr)
		if got == nil || want == nil || got.Error() != want.Error() {
			t.Errorf("Parse(%q): error %v, want go-nodeset's %v", expr, got, want)
		}
	}
}

// A group reference without a name is what @rack:$R leaves behind when R is
// empty. go-nodeset hands it to the resolver, and an attribute source
// answers it with every node that carries the attribute, so it is refused
// before the resolver is asked.
func TestAGroupReferenceWithoutANameIsRefused(t *testing.T) {
	t.Parallel()
	res := &asking{MapResolver: nodeset.NewMapResolver("rack", map[string]string{"": "exe[1-9]", "R1": "exe[1-2]"})}
	for expr, want := range map[string]string{
		"@":               "empty group name in @",
		"@:":              "empty group name in @:",
		"@rack:":          "empty group name in @rack:",
		"exe1,@rack:":     "empty group name in @rack:",
		"@rack:&exe1":     "empty group name in @rack:",
		"@rack:R1 @rack:": "empty group name in @rack:",
	} {
		ns, err := nodeexpr.ParseWith(expr, res)
		if err == nil || err.Error() != want {
			t.Errorf("ParseWith(%q) = %v, %v; want the error %q", expr, ns, err, want)
		}
	}
	if len(res.asked) > 0 {
		t.Errorf("the resolver was asked for %q", res.asked)
	}
	// Without a resolver every reference is refused, an empty one with it.
	if _, err := nodeexpr.Parse("@"); err == nil || err.Error() != "group @ cannot be resolved: no group source is configured" {
		t.Errorf("Parse(\"@\"): error %v, want go-nodeset's", err)
	}
}

// What a group names is held to the same rules, at every level.
func TestTheExpressionOfAGroupIsCheckedToo(t *testing.T) {
	t.Parallel()
	res := &nodeset.MapResolver{Default: "site", Groups: map[string]map[string]string{
		"site": {"bad": "exe[1-]", "empty": "@rack:", "outer": "@bad", "fine": "exe[1-2]"},
		"rack": {"R1": "exe[1-]"},
	}}
	for expr, want := range map[string]string{
		"@bad":       `group @bad: in "exe[1-]": the range "1-" has no last bound`,
		"@empty":     `group @empty: empty group name in @rack:`,
		"@outer":     `group @bad: in "exe[1-]": the range "1-" has no last bound`,
		"@rack:R1":   `group @rack:R1: in "exe[1-]": the range "1-" has no last bound`,
		"@rack:*":    `group @rack:*: in "exe[1-]": the range "1-" has no last bound`,
		"@fine,@bad": `group @bad: in "exe[1-]": the range "1-" has no last bound`,
	} {
		ns, err := nodeexpr.ParseWith(expr, res)
		if err == nil || err.Error() != want {
			t.Errorf("ParseWith(%q) = %v, %v; want the error %q", expr, ns, err, want)
		}
	}
	if ns, err := nodeexpr.ParseWith("@fine", res); err != nil || ns.String() != "exe[1-2]" {
		t.Errorf("ParseWith(\"@fine\") = %v, %v; want exe[1-2]", ns, err)
	}
}

// asking records the groups it was asked for.
type asking struct {
	*nodeset.MapResolver
	asked []string
}

func (a *asking) Resolve(source, group string) (string, error) {
	a.asked = append(a.asked, source+":"+group)
	return a.MapResolver.Resolve(source, group)
}
