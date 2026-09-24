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
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/GSI-HPC/clusterctl/nodeset"
)

// The output formats -o accepts.
const (
	FormatTable    = "table"
	FormatWide     = "wide"
	FormatJSON     = "json"
	FormatYAML     = "yaml"
	FormatNodeset  = "nodeset"
	FormatName     = "name"
	FormatJSONPath = "jsonpath"
	FormatJQ       = "jq"
)

// Format is a parsed -o value.
type Format struct {
	// Kind is one of the format constants.
	Kind string
	// Arg is the expression of a jsonpath or jq format.
	Arg string
}

// Formats lists the formats -o accepts, for the help text and the shell
// completion.
func Formats() []string {
	return []string{FormatTable, FormatWide, FormatJSON, FormatYAML,
		FormatNodeset, FormatName, FormatJSONPath + "=", FormatJQ + "="}
}

// ParseFormat reads a -o value.
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
	case FormatJSONPath, FormatJQ:
		if !hasArg || arg == "" {
			return Format{}, fmt.Errorf("the %s output format needs an expression, as -o %s='...'", kind, kind)
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
// which is when progress and decoration are left out.
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
	// Object is rendered by the json, yaml, jsonpath and jq formats. When it
	// is nil the table is converted to a list of objects.
	Object any
	// Nodes is rendered by the nodeset and name formats. When it is nil
	// those formats fall back to the first column of the table.
	Nodes *nodeset.NodeSet
}

// Write renders a result.
func (f Format) Write(w io.Writer, r Result) error {
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
	case FormatJSONPath:
		return writeJSONPath(w, f.Arg, r.object())
	case FormatJQ:
		return writeJQ(w, f.Arg, r.object())
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

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeYAML(w io.Writer, v any) error {
	// The value goes through JSON so that the json struct tags, which every
	// type in this program carries, decide the field names.
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return err
	}
	out, err := yaml.Marshal(generic)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
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

// nodes returns the node set a result names, falling back to the first column
// of its table.
func (r Result) nodes() (*nodeset.NodeSet, error) {
	if r.Nodes != nil {
		return r.Nodes, nil
	}
	ns := nodeset.New()
	if r.Table == nil {
		return ns, nil
	}
	for _, row := range r.Table.Rows {
		if len(row) == 0 || row[0] == "" {
			continue
		}
		if err := ns.Add(row[0]); err != nil {
			return nil, fmt.Errorf("the first column does not hold host names: %w", err)
		}
	}
	return ns, nil
}

// sortedKeys returns the keys of a map in a stable order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
