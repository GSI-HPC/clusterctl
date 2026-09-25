// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/output"
	"github.com/GSI-HPC/clusterctl/nodeset"
)

func sampleTable() *output.Table {
	t := output.NewTable(
		output.Column{Name: "NODE"},
		output.Column{Name: "STATE"},
		output.Column{Name: "REASON", Wide: true},
		output.Column{Name: "JOBS", Right: true},
	)
	t.Add("exe0001", "idle", "", "0")
	t.Add("exe0002", "drained", "firmware update, ticket 4711", "12")
	return t
}

func render(t *testing.T, spec string, r output.Result) string {
	t.Helper()
	f, err := output.ParseFormat(spec)
	if err != nil {
		t.Fatalf("ParseFormat(%q) failed: %v", spec, err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf, r); err != nil {
		t.Fatalf("writing %q: %v", spec, err)
	}
	return buf.String()
}

func TestParseFormat(t *testing.T) {
	t.Parallel()

	good := map[string]output.Format{
		"":               {Kind: output.FormatTable},
		"table":          {Kind: output.FormatTable},
		"wide":           {Kind: output.FormatWide},
		"json":           {Kind: output.FormatJSON},
		"nodeset":        {Kind: output.FormatNodeset},
		"jq=.[] | .node": {Kind: output.FormatJQ, Arg: ".[] | .node"},
	}
	for spec, want := range good {
		got, err := output.ParseFormat(spec)
		if err != nil {
			t.Errorf("ParseFormat(%q) failed: %v", spec, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFormat(%q) = %+v, want %+v", spec, got, want)
		}
	}

	for _, spec := range []string{"xml", "json=x", "jsonpath={.a}", "jq"} {
		if _, err := output.ParseFormat(spec); err == nil {
			t.Errorf("ParseFormat(%q) should fail", spec)
		}
	}
}

func TestTableOmitsWideColumns(t *testing.T) {
	t.Parallel()

	got := render(t, "table", output.Result{Table: sampleTable()})
	if strings.Contains(got, "REASON") {
		t.Errorf("the default table shows a wide column:\n%s", got)
	}
	if !strings.Contains(got, "NODE") || !strings.Contains(got, "exe0002") {
		t.Errorf("the table is missing its content:\n%s", got)
	}

	wide := render(t, "wide", output.Result{Table: sampleTable()})
	if !strings.Contains(wide, "firmware update, ticket 4711") {
		t.Errorf("-o wide does not show the reason:\n%s", wide)
	}
}

func TestTableAlignsAndDoesNotTruncate(t *testing.T) {
	t.Parallel()

	long := "a reason long enough that a fixed width column would have cut it off"
	tbl := output.NewTable(output.Cols("NODE", "REASON")...)
	tbl.Add("exe1", long)
	got := render(t, "table", output.Result{Table: tbl})
	if !strings.Contains(got, long) {
		t.Errorf("the value was truncated:\n%s", got)
	}
	for line := range strings.SplitSeq(strings.TrimRight(got, "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("line %q has trailing whitespace", line)
		}
	}
}

func TestJSONAndYAML(t *testing.T) {
	t.Parallel()

	js := render(t, "json", output.Result{Table: sampleTable()})
	for _, want := range []string{`"node": "exe0001"`, `"state": "drained"`, `"reason"`} {
		if !strings.Contains(js, want) {
			t.Errorf("-o json is missing %s:\n%s", want, js)
		}
	}

	ym := render(t, "yaml", output.Result{Table: sampleTable()})
	if !strings.Contains(ym, "node: exe0001") {
		t.Errorf("-o yaml is missing a field:\n%s", ym)
	}
}

func TestNodesetAndName(t *testing.T) {
	t.Parallel()

	r := output.Result{Table: sampleTable()}
	if got, want := render(t, "nodeset", r), "exe[0001-0002]\n"; got != want {
		t.Errorf("-o nodeset = %q, want %q", got, want)
	}
	if got, want := render(t, "name", r), "exe0001\nexe0002\n"; got != want {
		t.Errorf("-o name = %q, want %q", got, want)
	}

	explicit := output.Result{Nodes: nodeset.MustParse("sub[1-3]")}
	if got, want := render(t, "nodeset", explicit), "sub[1-3]\n"; got != want {
		t.Errorf("-o nodeset = %q, want %q", got, want)
	}
	if got := render(t, "nodeset", output.Result{}); got != "" {
		t.Errorf("an empty result printed %q", got)
	}
}

func TestJQ(t *testing.T) {
	t.Parallel()

	r := output.Result{Table: sampleTable()}
	got := render(t, "jq=.[] | select(.state == \"drained\") | .node", r)
	if got != "exe0002\n" {
		t.Errorf("jq output = %q, want %q", got, "exe0002\n")
	}

	if _, err := output.ParseFormat("jq=.[ |"); err == nil {
		t.Error("a malformed jq expression should be reported")
	}
}

func TestIsMachine(t *testing.T) {
	t.Parallel()

	for spec, want := range map[string]bool{
		"table": false, "wide": false,
		"json": true, "yaml": true, "nodeset": true, "name": true,
		"jq=.": true,
	} {
		f, err := output.ParseFormat(spec)
		if err != nil {
			t.Fatalf("ParseFormat(%q): %v", spec, err)
		}
		if got := f.IsMachine(); got != want {
			t.Errorf("IsMachine(%q) = %v, want %v", spec, got, want)
		}
		if f.String() != spec && spec != "" {
			t.Errorf("String() = %q, want %q", f.String(), spec)
		}
	}
}

func TestColsMarksColumnsByName(t *testing.T) {
	cols := output.Cols("NODE", "CPUS", "REASON").Wide("REASON").Right("CPUS")
	want := output.Columns{{Name: "NODE"}, {Name: "CPUS", Right: true}, {Name: "REASON", Wide: true}}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %+v, want %+v", cols, want)
	}
	defer func() {
		if recover() == nil {
			t.Error("a column that does not exist was marked without a panic")
		}
	}()
	output.Cols("NODE").Wide("NODES")
}
