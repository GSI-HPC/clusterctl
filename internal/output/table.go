// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import (
	"slices"
	"strings"

	"github.com/GSI-HPC/go-clikit/cliout"
)

// Column is one column of a table: cliout.Column.
type Column = cliout.Column

// Table is a command result in rows and columns: cliout.Table. Values are
// never truncated: a long reason or a wide node set is worth a wrapped line,
// and truncation has hidden the important half of a message often enough.
type Table = cliout.Table

// NewTable starts a table with the given headings.
func NewTable(columns ...Column) *Table {
	return &Table{Columns: columns}
}

// Columns are the headings of a table: plain ones from Cols, marked by Wide
// and Right.
type Columns []Column

// Cols builds plain columns from their names.
func Cols(names ...string) Columns {
	out := make(Columns, len(names))
	for i, n := range names {
		out[i] = Column{Name: n}
	}
	return out
}

// Wide shows the named columns only under -o wide.
func (c Columns) Wide(names ...string) Columns {
	return c.mark(names, func(col *Column) { col.Wide = true })
}

// Right aligns the values of the named columns to the right.
func (c Columns) Right(names ...string) Columns {
	return c.mark(names, func(col *Column) { col.Right = true })
}

// mark sets a property of the named columns. A name that is none of them is
// a mistake in the program, which every test of the table's command finds.
func (c Columns) mark(names []string, set func(*Column)) Columns {
	for _, name := range names {
		i := slices.IndexFunc(c, func(col Column) bool { return col.Name == name })
		if i < 0 {
			panic("output: the table has no column " + name)
		}
		set(&c[i])
	}
	return c
}

// Objects renders the table as a list of objects, which is what the machine
// formats show when a command has nothing richer to offer.
func Objects(t *Table) []map[string]string {
	if t == nil {
		return nil
	}
	out := make([]map[string]string, 0, len(t.Rows))
	for _, row := range t.Rows {
		obj := make(map[string]string, len(t.Columns))
		for i, col := range t.Columns {
			value := ""
			if i < len(row) {
				value = row[i]
			}
			obj[fieldName(col.Name)] = value
		}
		out = append(out, obj)
	}
	return out
}

// fieldName turns a heading into a field name: "EXIT CODE" becomes
// "exitCode".
func fieldName(heading string) string {
	parts := strings.FieldsFunc(strings.ToLower(heading), func(r rune) bool {
		return r == ' ' || r == '_' || r == '-'
	})
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(parts[0])
	for _, p := range parts[1:] {
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}
