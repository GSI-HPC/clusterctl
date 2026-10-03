// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package output renders command results in the format the caller asked for.
//
// Every command produces a Result holding the same information three ways: a
// table for a terminal, an object for a machine, and, where it makes sense, a
// node set. The format flag then picks one. A command never formats its own
// output, so -o json means the same thing everywhere.
package output

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"github.com/GSI-HPC/go-nodeset"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/nodeexpr"
)

// The output formats -o accepts.
const (
	FormatTable   = "table"
	FormatWide    = "wide"
	FormatJSON    = "json"
	FormatYAML    = "yaml"
	FormatNodeset = "nodeset"
	FormatName    = "name"
	FormatJQ      = "jq"
)

// Format is a parsed -o value.
type Format struct {
	// Kind is one of the format constants.
	Kind string
	// Arg is the program of the jq format.
	Arg string
}

// Formats lists the formats -o accepts, for the help text and the shell
// completion.
func Formats() []string {
	return []string{FormatTable, FormatWide, FormatJSON, FormatYAML,
		FormatNodeset, FormatName, FormatJQ + "="}
}

// ParseFormat reads a -o value. A jq program is compiled here, so that a
// mistake in it is reported before a command has acted rather than when its
// result is printed.
func ParseFormat(s string) (Format, error) {
	if s == "" {
		return Format{Kind: FormatTable}, nil
	}
	kind, arg, hasArg := strings.Cut(s, "=")
	switch kind {
	case FormatTable, FormatWide, FormatJSON, FormatYAML, FormatNodeset, FormatName:
		if hasArg {
			return Format{}, fmt.Errorf("the %s output format takes no argument", kind)
		}
		return Format{Kind: kind}, nil
	case FormatJQ:
		if !hasArg || arg == "" {
			return Format{}, fmt.Errorf("the %s output format needs an expression, as -o %s='...'", kind, kind)
		}
		if _, err := compileJQ(arg); err != nil {
			return Format{}, err
		}
		return Format{Kind: kind, Arg: arg}, nil
	default:
		return Format{}, fmt.Errorf("unknown output format %q; expected one of %s", s, strings.Join(Formats(), ", "))
	}
}

// String renders the format the way it was written.
func (f Format) String() string {
	if f.Arg != "" {
		return f.Kind + "=" + f.Arg
	}
	return f.Kind
}

// IsMachine reports whether the format is meant to be parsed by a program,
// which is when the decoration of standard output, such as a note between
// the rows, is left out. Progress has nothing to do with it: it is only
// ever drawn on standard error, whatever the format.
func (f Format) IsMachine() bool {
	switch f.Kind {
	case FormatTable, FormatWide:
		return false
	default:
		return true
	}
}

// Result is what a command produces, in every shape the formats need.
type Result struct {
	// Table is rendered by the table and wide formats.
	Table *Table
	// Object is rendered by the json, yaml and jq formats. When it is nil the
	// table is converted to a list of objects.
	Object any
	// Nodes is rendered by the nodeset and name formats. When it is nil
	// those formats fall back to the first column of the table, if that
	// column holds node or host names (nodeColumns), and refuse the result
	// otherwise.
	Nodes *nodeset.NodeSet
}

// nodeColumns are the headings of the columns that hold node or host names.
// Only a first column under one of them stands for the nodes a result lists:
// another, such as a job id, a file or an attribute value, would print as
// host names that name nothing.
var nodeColumns = []string{"NODE", "HOST", "BMC"}

// Check reports whether a result shaped like r can be printed in the
// format. Only -o nodeset and -o name refuse one, a result that lists no
// nodes. A command that changes something checks the shape of its result
// first, so that it is refused before the change rather than after it.
func (f Format) Check(r Result) error {
	switch f.Kind {
	case FormatNodeset, FormatName:
		_, err := r.nodes()
		return err
	}
	return nil
}

// Write renders a result.
func (f Format) Write(w io.Writer, r Result) error {
	return f.WriteContext(context.Background(), w, r)
}

// WriteContext renders a result, giving up when ctx ends. Only a jq program
// can run for long enough to need it.
func (f Format) WriteContext(ctx context.Context, w io.Writer, r Result) error {
	switch f.Kind {
	case FormatTable:
		return writeTable(w, r.Table, false)
	case FormatWide:
		return writeTable(w, r.Table, true)
	case FormatJSON:
		return writeJSON(w, r.object())
	case FormatYAML:
		return writeYAML(w, r.object())
	case FormatNodeset:
		return writeNodeset(w, r)
	case FormatName:
		return writeNames(w, r)
	case FormatJQ:
		return writeJQ(ctx, w, f.Arg, r.object())
	default:
		return fmt.Errorf("unknown output format %q", f.Kind)
	}
}

// object returns what the machine formats render.
func (r Result) object() any {
	if r.Object != nil {
		return r.Object
	}
	if r.Table != nil {
		return r.Table.Objects()
	}
	return map[string]any{}
}

// writeJSON renders v as JSON indented by two spaces. A list is written an
// element at a time, each encoded on its own: encoding the whole of what a
// thousand nodes printed held it twice over, once encoded and once
// indented, on top of the results themselves. The bytes are those the
// whole would have been encoded to.
func writeJSON(w io.Writer, v any) error {
	list := reflect.ValueOf(v)
	if !elementwise(list) {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if _, err := io.WriteString(w, "[\n"); err != nil {
		return err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("  ", "  ")
	for i := range list.Len() {
		b.Reset()
		b.WriteString("  ")
		// An element of a list can be addressed, so a MarshalJSON of its
		// pointer is what encodes it there; it is handed over by address
		// so that it is here too.
		if err := enc.Encode(list.Index(i).Addr().Interface()); err != nil {
			return err
		}
		// Encode ends the element with a newline, which follows the
		// comma that separates it from the next.
		if i < list.Len()-1 {
			b.Truncate(b.Len() - 1)
			b.WriteString(",\n")
		}
		if _, err := w.Write(b.Bytes()); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]\n")
	return err
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

// elementwise reports whether writeJSON writes v an element at a time: a
// slice with elements, which encoding/json renders as a JSON array of
// them, so not a []byte, which it renders as a string, nor a slice type
// that marshals itself.
func elementwise(v reflect.Value) bool {
	if v.Kind() != reflect.Slice || v.Len() == 0 || v.Type().Elem().Kind() == reflect.Uint8 {
		return false
	}
	t := v.Type()
	for _, m := range []reflect.Type{jsonMarshaler, textMarshaler} {
		if t.Implements(m) || reflect.PointerTo(t).Implements(m) {
			return false
		}
	}
	return true
}

func writeNodeset(w io.Writer, r Result) error {
	ns, err := r.nodes()
	if err != nil {
		return err
	}
	if ns.IsEmpty() {
		return nil
	}
	_, err = fmt.Fprintln(w, EscapeCell(ns.String()))
	return err
}

func writeNames(w io.Writer, r Result) error {
	ns, err := r.nodes()
	if err != nil {
		return err
	}
	for _, name := range ns.Expand() {
		if _, err := fmt.Fprintln(w, EscapeCell(name)); err != nil {
			return err
		}
	}
	return nil
}

// nodes returns the node set a result lists: its Nodes, else the first
// column of its table when that column holds node or host names.
func (r Result) nodes() (*nodeset.NodeSet, error) {
	if r.Nodes != nil {
		return r.Nodes, nil
	}
	if r.Table == nil || len(r.Table.Columns) == 0 || !slices.Contains(nodeColumns, r.Table.Columns[0].Name) {
		return nil, exitcode.Errorf(exitcode.Usage,
			"-o nodeset and -o name print the nodes or hosts a command lists, and this command lists none; use -o table or -o json")
	}
	ns := nodeset.New()
	for _, row := range r.Table.Rows {
		if len(row) == 0 || row[0] == "" {
			continue
		}
		if err := nodeexpr.Add(ns, row[0]); err != nil {
			return nil, fmt.Errorf("the %s column does not hold host names: %w", r.Table.Columns[0].Name, err)
		}
	}
	return ns, nil
}
