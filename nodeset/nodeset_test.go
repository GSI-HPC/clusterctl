// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package nodeset_test

import (
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/nodeset"
)

func TestParseExpand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		expr string
		want []string
	}{
		{"exe0001", []string{"exe0001"}},
		{"login", []string{"login"}},
		{"exe[1-3]", []string{"exe1", "exe2", "exe3"}},
		{"exe[0001-0003]", []string{"exe0001", "exe0002", "exe0003"}},
		{"exe[1-10/3]", []string{"exe1", "exe4", "exe7", "exe10"}},
		{"exe[1,5,9]", []string{"exe1", "exe5", "exe9"}},
		{"exe[09-11]", []string{"exe09", "exe10", "exe11"}},
		{"exe[1-2]-ib[0-1]", []string{"exe1-ib0", "exe1-ib1", "exe2-ib0", "exe2-ib1"}},
		{"rack[1-2]node[01-02]", []string{"rack1node01", "rack1node02", "rack2node01", "rack2node02"}},
		{"exe1.hpc.example.org", []string{"exe1.hpc.example.org"}},
		{"exe[1-2].hpc.example.org", []string{"exe1.hpc.example.org", "exe2.hpc.example.org"}},
		{"exe[1-3],sub[1-2]", []string{"exe1", "exe2", "exe3", "sub1", "sub2"}},
		{"exe[1-3] sub1", []string{"exe1", "exe2", "exe3", "sub1"}},
		{"exe[1-5]!exe[2-3]", []string{"exe1", "exe4", "exe5"}},
		{"exe[1-5]&exe[4-8]", []string{"exe4", "exe5"}},
		{"exe[1-3]^exe[2-4]", []string{"exe1", "exe4"}},
		{"exe[1-3]!exe2,sub1", []string{"exe1", "exe3", "sub1"}},
		{"", nil},
		// Padding is not part of a host's identity, so these name one host,
		// shown the way it was first written.
		{"exe1,exe01", []string{"exe1"}},
		{"exe01,exe1", []string{"exe01"}},
		{"exe[1,01]", []string{"exe1"}},
		// Every host keeps its own width, so none is shown under a name it
		// was not given.
		{"exe[01-02],exe3", []string{"exe01", "exe02", "exe3"}},
		{"exe[0001-0002,11]", []string{"exe0001", "exe0002", "exe11"}},
		{"exe[01-100/99]", []string{"exe01", "exe100"}},
		{"exe[001-010]", []string{"exe001", "exe002", "exe003", "exe004", "exe005", "exe006", "exe007", "exe008", "exe009", "exe010"}},
		{"exe[08-10]!exe09", []string{"exe08", "exe10"}},
		{"exe1 & exe[1-2]", []string{"exe1"}},
		{"exe[1-3],,exe5,", []string{"exe1", "exe2", "exe3", "exe5"}},
	}

	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			t.Parallel()
			ns, err := nodeset.Parse(tc.expr)
			if err != nil {
				t.Fatalf("Parse(%q) failed: %v", tc.expr, err)
			}
			got := ns.Expand()
			if !equal(got, tc.want) {
				t.Errorf("Parse(%q).Expand() = %v, want %v", tc.expr, got, tc.want)
			}
			if ns.Len() != len(tc.want) {
				t.Errorf("Len() = %d, want %d", ns.Len(), len(tc.want))
			}
		})
	}
}

func TestFold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		expr string
		want string
	}{
		{"single host keeps its name", "exe1", "exe1"},
		{"non numeric host is untouched", "login", "login"},
		{"contiguous run folds", "exe1 exe2 exe3", "exe[1-3]"},
		{"padding is preserved", "exe0001 exe0002", "exe[0001-0002]"},
		{"gaps are listed", "exe1 exe2 exe5", "exe[1-2,5]"},
		{"pads do not merge across widths", "exe09 exe10", "exe[09-10]"},
		{"the first spelling of a host wins", "exe1 exe01", "exe1"},
		{"every host keeps its width", "exe[01-02] exe3", "exe[01-02,3]"},
		{"a stray unpadded host is not renamed", "exe[0001-0010] exe11", "exe[0001-0010,11]"},
		{"a wider value joins a padded run", "exe08 exe09 exe10 exe11", "exe[08-11]"},
		{"widths split a run", "exe7 exe08 exe9", "exe[7,08,9]"},
		{"patterns are listed alphabetically", "sub1 exe1", "exe1,sub1"},
		{"a single valued dimension loses its brackets", "exe[1-1]", "exe1"},
		{"two dimensions fold into one vector", "exe1-ib0 exe1-ib1 exe2-ib0 exe2-ib1", "exe[1-2]-ib[0-1]"},
		// Folding merges along the leftmost dimension first, so a ragged set
		// splits on the dimension that varies last.
		{"a ragged set folds on the first dimension", "exe1-ib0 exe1-ib1 exe2-ib0", "exe[1-2]-ib0,exe1-ib1"},
		{"domains fold on the host part", "exe1.hpc.example.org exe2.hpc.example.org", "exe[1-2].hpc.example.org"},
		{"an empty set renders empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ns, err := nodeset.Parse(tc.expr)
			if err != nil {
				t.Fatalf("Parse(%q) failed: %v", tc.expr, err)
			}
			if got := ns.String(); got != tc.want {
				t.Errorf("Parse(%q).String() = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}

func TestFoldRoundTrip(t *testing.T) {
	t.Parallel()

	exprs := []string{
		"exe[1-10]", "exe[0001-0016],sub[01-02],login",
		"exe[1-2]-ib[0-1]", "exe1,exe01,exe001",
		"rack[1-3]node[01-04]", "10.0.1.[1-8]",
		"exe[1-5]!exe3", "exe[1-100]",
		"exe[01-02],exe3", "exe[0001-0010],exe11", "exe7,exe08,exe9",
		"exe[08-12]!exe[08-09]", "x[1-2]y[01-02],x3y3", "exe[00-01],exe0",
	}
	for _, expr := range exprs {
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			first, err := nodeset.Parse(expr)
			if err != nil {
				t.Fatalf("Parse(%q) failed: %v", expr, err)
			}
			folded := first.String()
			second, err := nodeset.Parse(folded)
			if err != nil {
				t.Fatalf("Parse(%q) of folded form failed: %v", folded, err)
			}
			if got := second.String(); got != folded {
				t.Errorf("folding is not stable: %q then %q", folded, got)
			}
			if !equal(first.Expand(), second.Expand()) {
				t.Errorf("folding lost hosts: %v vs %v", first.Expand(), second.Expand())
			}
		})
	}
}

// TestFoldLargeSets folds sets far larger than any cluster. Folding has to stay
// close to linear: merging a dimension one host at a time and sorting it after
// every merge turns this test from milliseconds into minutes.
func TestFoldLargeSets(t *testing.T) {
	t.Parallel()

	for _, expr := range []string{"exe[1-262144]", "rack[1-256]-exe[1-256]"} {
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			if got := nodeset.MustParse(expr).String(); got != expr {
				t.Errorf("String() = %.60q, want %q", got, expr)
			}
		})
	}
}

func TestAutostep(t *testing.T) {
	t.Parallel()

	ns, err := nodeset.Parse("exe[1,3,5,7,9]", nodeset.WithAutostep(3))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if got, want := ns.String(), "exe[1-9/2]"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	plain, err := nodeset.Parse("exe[1,3,5,7,9]")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if got, want := plain.String(), "exe[1,3,5,7,9]"; got != want {
		t.Errorf("without autostep String() = %q, want %q", got, want)
	}
}

func TestSetOperations(t *testing.T) {
	t.Parallel()

	a := nodeset.MustParse("exe[1-5]")
	b := nodeset.MustParse("exe[4-8]")

	cases := []struct {
		name string
		got  *nodeset.NodeSet
		want string
	}{
		{"union", a.Union(b), "exe[1-8]"},
		{"intersection", a.Intersection(b), "exe[4-5]"},
		{"difference", a.Difference(b), "exe[1-3]"},
		{"symmetric difference", a.SymmetricDifference(b), "exe[1-3,6-8]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got.String(); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
	if got, want := a.String(), "exe[1-5]"; got != want {
		t.Errorf("the operands were modified: %q, want %q", got, want)
	}
}

func TestOperatorsEvaluateLeftToRight(t *testing.T) {
	t.Parallel()

	// (exe[1-10] minus exe[1-5]) intersected with exe[1-7] is exe[6-7];
	// grouping the operators any other way gives a different answer.
	ns := nodeset.MustParse("exe[1-10]!exe[1-5]&exe[1-7]")
	if got, want := ns.String(), "exe[6-7]"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestGroups(t *testing.T) {
	t.Parallel()

	res := &nodeset.MapResolver{
		Groups: map[string]map[string]string{
			"local": {
				"compute": "exe[1-4]",
				"submit":  "sub[1-2]",
				"all":     "@compute,@submit",
				"empty":   "",
			},
			"slurm": {"idle": "exe[3-4]"},
		},
		Default: "local",
	}

	tests := []struct {
		expr string
		want string
	}{
		{"@compute", "exe[1-4]"},
		{"@local:compute", "exe[1-4]"},
		{"@slurm:idle", "exe[3-4]"},
		{"@all", "exe[1-4],sub[1-2]"},
		{"@compute!@slurm:idle", "exe[1-2]"},
		{"@compute,login", "exe[1-4],login"},
		{"@empty", ""},
		{"@*", "exe[1-4],sub[1-2]"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			t.Parallel()
			ns, err := nodeset.ParseWith(tc.expr, res)
			if err != nil {
				t.Fatalf("ParseWith(%q) failed: %v", tc.expr, err)
			}
			if got := ns.String(); got != tc.want {
				t.Errorf("ParseWith(%q) = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}

func TestGroupErrors(t *testing.T) {
	t.Parallel()

	if _, err := nodeset.Parse("@compute"); err == nil {
		t.Error("Parse of a group without a resolver should fail")
	}

	cyclic := nodeset.NewMapResolver("local", map[string]string{"loop": "@loop"})
	if _, err := nodeset.ParseWith("@loop", cyclic); err == nil {
		t.Error("a group cycle should be reported")
	}

	res := nodeset.NewMapResolver("local", map[string]string{"compute": "exe[1-2]"})
	for _, expr := range []string{"@missing", "@other:compute", "@"} {
		if _, err := nodeset.ParseWith(expr, res); err == nil {
			t.Errorf("ParseWith(%q) should fail", expr)
		}
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	exprs := []string{
		"exe[1-", "exe1]", "exe[]", "exe[5-1]", "exe[1-2/0]",
		"exe[a-b]", "exe[1-2/x]", "exe[3/2]", "exe[1,,2]",
		"exe[1-100000000]",
		// A step is a plain decimal number like a bound, so it can neither
		// carry a sign nor be large enough to wrap around.
		"exe[1-9/+2]", "exe[5-999999999999999999/9223372036854775807]",
		// Adjacent numeric parts cannot be told apart once expanded.
		"exe0[0,10]", "exe[1-2][3-4]", "[1-2]0",
		// A last bound padded differently from the first would be shown
		// under a name it was not given.
		"exe[1-010]", "exe[001-10]", "exe[01-005]",
		// A set operator needs an operand on each side. A dangling one is
		// what an empty command substitution leaves behind, and dropping it
		// would select every host of the left operand.
		"exe[1-3]&", "exe[1-3]!", "exe[1-3]^", "exe[1-3]! ",
		"exe[1-3]!,exe2", "exe[1-3]&&exe2", "exe[1-3]&!exe2", "exe[1-3],&exe2",
		"&exe1", "!exe1", " ^exe1",
		// A host name never begins with a dash; ssh would read it as an
		// option.
		"-oProxyCommand=x", "exe1 -exe2", "exe1,-exe[1-2]",
	}
	for _, expr := range exprs {
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			if _, err := nodeset.Parse(expr); err == nil {
				t.Errorf("Parse(%q) should fail", expr)
			}
		})
	}
}

func TestHostlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		expr string
		want string
	}{
		{"exe[1-4]", "exe[1-4]"},
		{"exe[1-2],sub1", "exe[1-2],sub1"},
		// Every name has one bracketed range at most, folded along the
		// dimension that gives the fewest names, the last one on a tie.
		{"exe[1-2]-ib[0-1]", "exe1-ib[0-1],exe2-ib[0-1]"},
		{"r[01-02]n[001-100]", "r01n[001-100],r02n[001-100]"},
		{"exe[0001-0100].mgmt.dc2.example.org", "exe[0001-0100].mgmt.dc2.example.org"},
		// Slurm and FreeIPMI read no steps, whatever the set was parsed with.
		{"exe[1,3,5,7]", "exe[1,3,5,7]"},
		{"exe[0001-0010],exe11", "exe[0001-0010,11]"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			t.Parallel()
			ns := nodeset.MustParse(tc.expr, nodeset.WithAutostep(2))
			if got := ns.Hostlist(); got != tc.want {
				t.Errorf("Hostlist() = %q, want %q", got, tc.want)
			}
			back := nodeset.MustParse(ns.Hostlist())
			if !equal(back.Expand(), ns.Expand()) {
				t.Errorf("Hostlist() %q names %v, want %v", ns.Hostlist(), back.Expand(), ns.Expand())
			}
		})
	}
}

func TestSplit(t *testing.T) {
	t.Parallel()

	ns := nodeset.MustParse("exe[1-7]")
	chunks := ns.Split(3)
	if len(chunks) != 3 {
		t.Fatalf("Split(3) returned %d chunks, want 3", len(chunks))
	}
	want := []string{"exe[1-3]", "exe[4-5]", "exe[6-7]"}
	total := 0
	for i, c := range chunks {
		if got := c.String(); got != want[i] {
			t.Errorf("chunk %d = %q, want %q", i, got, want[i])
		}
		total += c.Len()
	}
	if total != ns.Len() {
		t.Errorf("chunks hold %d hosts, want %d", total, ns.Len())
	}
	if got := ns.Split(0); got != nil {
		t.Errorf("Split(0) = %v, want nil", got)
	}
	if got := nodeset.New().Split(2); got != nil {
		t.Errorf("splitting an empty set = %v, want nil", got)
	}
	if got := len(ns.Split(100)); got != ns.Len() {
		t.Errorf("Split(100) returned %d chunks, want %d", got, ns.Len())
	}
}

func TestAddAndContains(t *testing.T) {
	t.Parallel()

	ns := nodeset.New()
	if err := ns.Add("exe[1-2]"); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := ns.Add("sub1"); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if !ns.Contains("exe1") || !ns.Contains("sub1") {
		t.Errorf("Contains missed a member of %q", ns)
	}
	if !ns.Contains("exe01") {
		t.Error("exe01 and exe1 name the same host, so Contains must match")
	}
	if ns.Contains("exe9") || ns.Contains("not a host[") {
		t.Error("Contains matched a host that is not a member")
	}
	if err := ns.Add("exe["); err == nil {
		t.Error("Add of a malformed expression should fail")
	}
	if ns.IsEmpty() {
		t.Error("IsEmpty on a populated set")
	}
}

func TestMustParsePanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("MustParse should panic on a malformed expression")
		}
	}()
	nodeset.MustParse("exe[")
}

func TestLargeSetIsRejected(t *testing.T) {
	t.Parallel()

	if _, err := nodeset.Parse("exe[1-2000]x[1-2000]"); err == nil {
		t.Error("a set beyond the element cap should be rejected")
	} else if !strings.Contains(err.Error(), "hosts") {
		t.Errorf("error = %v, want it to mention the host count", err)
	}
}

// TestExpressionLimits lowers the limits, so it must not run in parallel.
func TestExpressionLimits(t *testing.T) {
	nodeset.LowerLimits(t, 100)

	for _, expr := range []string{
		"exe[1-100]", "exe[1-50,51-100]", "exe[1-10]-ib[1-10]",
		"exe[1-50],exe[1-50]", "exe[1-50]!exe[1-25],sub[1-25]",
	} {
		if _, err := nodeset.Parse(expr); err != nil {
			t.Errorf("Parse(%q) failed within the limits: %v", expr, err)
		}
	}

	rejected := []struct{ expr, mentions string }{
		{"exe[1-101]", "elements"},
		// A range written as several parts is capped as a whole.
		{"exe[1-60,61-120]", "elements"},
		{"exe[1-10]-ib[1-11]", "hosts"},
		// So is an expression made of several terms, at every step.
		{"exe[1-60],sub[1-60]", "hosts"},
		{"exe[1-60] sub[1-60]!sub[1-60]", "hosts"},
		// Every term counts, however small the result stays, so the work
		// an expression costs is bounded as well as its size.
		{"exe[1-100]!exe[1-100],exe1", "together"},
		{"a[1-60]!a[1-60],b1", "together"},
		// A pattern is weighed one dimension at a time, before the next is
		// expanded.
		{"a[1-2]b[1-2]c[1-2]d[1-2]e[1-2]f[1-2]g[1-2]h[1-2]", "hosts"},
	}
	for _, tc := range rejected {
		_, err := nodeset.Parse(tc.expr)
		if err == nil {
			t.Errorf("Parse(%q) should exceed the limits", tc.expr)
			continue
		}
		if !strings.Contains(err.Error(), tc.mentions) {
			t.Errorf("Parse(%q) error = %v, want it to mention %s", tc.expr, err, tc.mentions)
		}
	}

	res := nodeset.NewMapResolver("local", map[string]string{
		"big": "exe[1-60],sub[1-60]", "half": "exe[1-60]",
	})
	if _, err := nodeset.ParseWith("@big", res); err == nil {
		t.Error("a group beyond the limits should be rejected")
	}
	// The terms of a group count towards the expression that refers to it.
	if _, err := nodeset.ParseWith("@half!@half,@half", res); err == nil {
		t.Error("groups beyond the limits together should be rejected")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestOversizedExpressionsAreRefusedEarly runs the two expressions that used
// to be refused, or accepted, only after gigabytes had been allocated: a term
// of two hundred full brackets, and a difference that cancels out repeated.
// Neither may cost more than the largest set an expression may name. It
// measures allocation across the whole process, so it must not run in
// parallel.
func TestOversizedExpressionsAreRefusedEarly(t *testing.T) {
	full := "[0-1048575]"
	allocated := func(expr string) (uint64, error) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := nodeset.Parse(expr)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc, err
	}

	largest, err := allocated("a" + full)
	if err != nil {
		t.Fatalf("the largest set was refused: %v", err)
	}
	for _, expr := range []string{
		"a" + strings.Repeat(full+"a", 200),
		strings.Repeat("a"+full+"!a"+full+",", 3),
	} {
		spent, err := allocated(expr)
		if err == nil {
			t.Errorf("Parse(%.40q...) should be refused", expr)
		}
		if spent > largest+largest/2 {
			t.Errorf("Parse(%.40q...) allocated %d MiB before refusing, the largest set costs %d MiB",
				expr, spent>>20, largest>>20)
		}
	}
}

// TestCanonicalReturnsAHeldName covers a set that holds hosts of one pattern
// written with different widths. Canonical used to render every member at one
// width per pattern, so it answered with names the set was never given.
func TestCanonicalReturnsAHeldName(t *testing.T) {
	t.Parallel()

	ns := nodeset.MustParse("exe[0001-0010],exe11,lab1")
	tests := []struct{ ask, want string }{
		{"exe1", "exe0001"},
		{"exe0001", "exe0001"},
		{"exe11", "exe11"},
		{"exe0011", "exe11"},
		{"lab01", "lab1"},
	}
	for _, tc := range tests {
		got, ok := ns.Canonical(tc.ask)
		if !ok || got != tc.want {
			t.Errorf("Canonical(%q) = %q, %v, want %q", tc.ask, got, ok, tc.want)
		}
	}
	for _, name := range []string{"exe12", "exe[1-2]", "-exe1", ""} {
		if got, ok := ns.Canonical(name); ok {
			t.Errorf("Canonical(%q) = %q, want no match", name, got)
		}
	}

	// Combining sets keeps the spelling already held.
	u := nodeset.MustParse("lab1").Union(nodeset.MustParse("lab01,lab2"))
	if got, want := u.String(), "lab[1-2]"; got != want {
		t.Errorf("union = %q, want %q", got, want)
	}
	for _, chunk := range ns.Split(2) {
		for _, name := range chunk.Expand() {
			if !contains(ns.Expand(), name) {
				t.Errorf("Split renamed a host to %q", name)
			}
		}
	}
}

func contains(names []string, name string) bool {
	return slices.Contains(names, name)
}
