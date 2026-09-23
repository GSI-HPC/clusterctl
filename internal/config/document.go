// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package config finds, validates, merges and resolves the clusterctl
// configuration.
//
// Each file is a stream of YAML documents. A document is decoded into a plain
// tree first, validated against the schema of its kind, and only then merged,
// so that a typo is reported at the line it was written on rather than
// wherever the merged value happens to be used.
package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// Origin says where a value came from.
type Origin struct {
	// Layer names the merge layer, "site", "cluster", "context" and so on.
	Layer string `json:"layer" yaml:"layer"`
	// File is the file the value was written in, empty for values that were
	// not read from a file.
	File string `json:"file,omitempty" yaml:"file,omitempty"`
	// Line and Column locate the value in that file.
	Line   int `json:"line,omitempty" yaml:"line,omitempty"`
	Column int `json:"column,omitempty" yaml:"column,omitempty"`
}

// String renders the origin the way config view --show-sources prints it.
func (o Origin) String() string {
	where := o.File
	if where != "" && o.Line > 0 {
		where = fmt.Sprintf("%s:%d:%d", o.File, o.Line, o.Column)
	}
	switch {
	case o.Layer == "" && where == "":
		return "unknown"
	case o.Layer == "":
		return where
	case where == "":
		return o.Layer
	default:
		return fmt.Sprintf("%s (%s)", o.Layer, where)
	}
}

// Document is one YAML document together with the positions of its values.
type Document struct {
	// File is the file the document was read from.
	File string
	// Index is its position in that file, counted from zero.
	Index int
	// APIVersion and Kind are taken from the document itself.
	APIVersion string
	Kind       string
	// Data is the document as a plain tree of map[string]any, []any and
	// scalars.
	Data map[string]any
	// Positions maps a dotted path to the position its value was written at.
	Positions map[string]Origin
}

// Position returns the origin of a path inside the document, falling back to
// the closest parent that is known and finally to the document itself.
func (d *Document) Position(path string) Origin {
	for p := path; ; {
		if o, ok := d.Positions[p]; ok {
			return o
		}
		cut := strings.LastIndexAny(p, ".[")
		if cut < 0 {
			break
		}
		p = p[:cut]
	}
	return Origin{File: d.File}
}

// ParseDocuments reads a YAML stream into documents, recording where every
// value was written.
func ParseDocuments(file string, data []byte) ([]*Document, error) {
	f, err := parser.ParseBytes(data, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}

	docs := make([]*Document, 0, len(f.Docs))
	for i, astDoc := range f.Docs {
		if astDoc.Body == nil {
			continue
		}
		doc := &Document{File: file, Index: i, Positions: map[string]Origin{}}
		value, err := doc.convert(astDoc.Body, "")
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", file, i+1, err)
		}
		tree, ok := value.(map[string]any)
		if !ok {
			if value == nil {
				continue
			}
			return nil, fmt.Errorf("%s: document %d: a configuration document must be a mapping", file, i+1)
		}
		doc.Data = tree
		doc.APIVersion, _ = tree["apiVersion"].(string)
		doc.Kind, _ = tree["kind"].(string)
		docs = append(docs, doc)
	}
	return docs, nil
}

// convert turns a YAML node into a plain tree, recording positions as it
// goes.
func (d *Document) convert(node ast.Node, path string) (any, error) {
	switch n := node.(type) {
	case *ast.DocumentNode:
		return d.convert(n.Body, path)
	case *ast.MappingNode:
		out := make(map[string]any, len(n.Values))
		for _, v := range n.Values {
			if err := d.convertPair(v, path, out); err != nil {
				return nil, err
			}
		}
		return out, nil
	case *ast.MappingValueNode:
		out := make(map[string]any, 1)
		if err := d.convertPair(n, path, out); err != nil {
			return nil, err
		}
		return out, nil
	case *ast.SequenceNode:
		out := make([]any, 0, len(n.Values))
		for i, v := range n.Values {
			item, err := d.convert(v, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	case *ast.AnchorNode:
		return d.convert(n.Value, path)
	case *ast.AliasNode:
		return nil, fmt.Errorf("anchors and aliases are not supported in the configuration (at %s)", path)
	case *ast.TagNode:
		return d.convert(n.Value, path)
	case *ast.NullNode:
		return nil, nil
	case *ast.BoolNode:
		return n.Value, nil
	case *ast.FloatNode:
		return n.Value, nil
	case *ast.IntegerNode:
		return integerValue(n), nil
	case *ast.StringNode:
		return n.Value, nil
	case *ast.LiteralNode:
		return n.Value.Value, nil
	case *ast.InfinityNode, *ast.NanNode:
		return nil, fmt.Errorf("the value at %s is not a number the configuration accepts", path)
	default:
		return nil, fmt.Errorf("unsupported YAML construct at %s", path)
	}
}

// convertPair converts one key and value of a mapping.
func (d *Document) convertPair(pair *ast.MappingValueNode, path string, out map[string]any) error {
	key, ok := pair.Key.(*ast.StringNode)
	if !ok {
		return fmt.Errorf("a mapping key must be a string (at %s)", path)
	}
	child := key.Value
	if path != "" {
		child = path + "." + key.Value
	}
	if _, exists := out[key.Value]; exists {
		return fmt.Errorf("duplicate key %q at %s", key.Value, child)
	}
	value, err := d.convert(pair.Value, child)
	if err != nil {
		return err
	}
	out[key.Value] = value
	pos := key.GetToken().Position
	d.Positions[child] = Origin{File: d.File, Line: pos.Line, Column: pos.Column}
	return nil
}

// integerValue reads an integer scalar.
//
// A literal written with a leading zero is kept as a string. YAML
// implementations disagree about whether "0600" is six hundred or octal three
// hundred and eighty-four, and a file mode silently becoming 384 is the kind
// of bug that only shows up on the node. Fields that take a mode are declared
// as strings for the same reason.
func integerValue(n *ast.IntegerNode) any {
	lit := n.GetToken().Value
	trimmed := strings.TrimLeft(lit, "+-")
	if len(trimmed) > 1 && trimmed[0] == '0' && isDigits(trimmed) {
		return lit
	}
	switch v := n.Value.(type) {
	case uint64:
		if v <= 1<<63-1 {
			return int64(v)
		}
		return strconv.FormatUint(v, 10)
	case int64:
		return v
	default:
		return n.Value
	}
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}
