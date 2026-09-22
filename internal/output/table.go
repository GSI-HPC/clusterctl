// SPDX-License-Identifier: LGPL-3.0-or-later

package output

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Column is one column of a table.
type Column struct {
	// Name is the heading, upper case by convention.
	Name string
	// Wide keeps the column out of the default table and shows it only
	// under -o wide.
	Wide bool
	// Right aligns the values to the right, for counts.
	Right bool
}

// Table is a command result in rows and columns. Values are never truncated:
// a long reason or a wide node set is worth a wrapped line, and truncation
// has hidden the important half of a message often enough.
type Table struct {
	Columns []Column
	Rows    [][]string
	// Caption is printed under the table, for a total or a note.
	Caption string
}

// NewTable starts a table with the given headings.
func NewTable(columns ...Column) *Table {
	return &Table{Columns: columns}
}

// Cols builds plain columns from their names.
func Cols(names ...string) []Column {
	out := make([]Column, len(names))
	for i, n := range names {
		out[i] = Column{Name: n}
	}
	return out
}

// Add appends a row. Missing cells are left empty and extra cells are kept,
// so that a column added later does not need every call site changed.
func (t *Table) Add(values ...string) {
	t.Rows = append(t.Rows, values)
}

// Len reports the number of rows.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.Rows)
}

// Objects renders the table as a list of objects, which is what the machine
// formats show when a command has nothing richer to offer.
func (t *Table) Objects() []map[string]string {
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

func writeTable(w io.Writer, t *Table, wide bool) error {
	if t == nil || len(t.Columns) == 0 {
		return nil
	}
	keep := make([]int, 0, len(t.Columns))
	for i, col := range t.Columns {
		if col.Wide && !wide {
			continue
		}
		keep = append(keep, i)
	}
	if len(keep) == 0 {
		return nil
	}

	widths := make([]int, len(keep))
	for j, i := range keep {
		widths[j] = utf8.RuneCountInString(t.Columns[i].Name)
	}
	for _, row := range t.Rows {
		for j, i := range keep {
			if i < len(row) {
				if n := utf8.RuneCountInString(row[i]); n > widths[j] {
					widths[j] = n
				}
			}
		}
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		for j := range keep {
			value := ""
			if j < len(cells) {
				value = cells[j]
			}
			if j > 0 {
				b.WriteString("  ")
			}
			pad := widths[j] - utf8.RuneCountInString(value)
			last := j == len(keep)-1
			switch {
			case t.Columns[keep[j]].Right:
				b.WriteString(strings.Repeat(" ", max(pad, 0)))
				b.WriteString(value)
			case last:
				// The last column is not padded, so that copying a line does
				// not pick up trailing spaces.
				b.WriteString(value)
			default:
				b.WriteString(value)
				b.WriteString(strings.Repeat(" ", max(pad, 0)))
			}
		}
		b.WriteByte('\n')
	}

	headings := make([]string, len(keep))
	for j, i := range keep {
		headings[j] = t.Columns[i].Name
	}
	writeRow(headings)

	cells := make([]string, len(keep))
	for _, row := range t.Rows {
		for j, i := range keep {
			if i < len(row) {
				cells[j] = row[i]
			} else {
				cells[j] = ""
			}
		}
		writeRow(cells)
	}
	if t.Caption != "" {
		fmt.Fprintf(&b, "\n%s\n", t.Caption)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
