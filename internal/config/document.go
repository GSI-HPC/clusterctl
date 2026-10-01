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
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []*Document
	for i := 0; ; i++ {
		var root yaml.Node
		if err := dec.Decode(&root); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		doc := &Document{File: file, Index: i, Positions: map[string]Origin{}}
		value, err := doc.convert(&root, "")
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
func (d *Document) convert(n *yaml.Node, path string) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return d.convert(n.Content[0], path)
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := d.convertPair(n.Content[i], n.Content[i+1], path, out); err != nil {
				return nil, err
			}
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, v := range n.Content {
			item, err := d.convert(v, path+"["+strconv.Itoa(i)+"]")
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	case yaml.AliasNode:
		return nil, fmt.Errorf("anchors and aliases are not supported in the configuration (at %s)", path)
	case yaml.ScalarNode:
		return scalarValue(n, path)
	default:
		return nil, fmt.Errorf("unsupported YAML construct at %s", path)
	}
}

// convertPair converts one key and value of a mapping. A key has to read as
// text, untagged: a key 8 or true would be a number or a boolean in some
// other reader's eyes.
func (d *Document) convertPair(key, value *yaml.Node, path string, out map[string]any) error {
	if key.Kind != yaml.ScalarNode || key.Style&yaml.TaggedStyle != 0 || scalarTag(key) != strTag || key.ShortTag() == mergeTag {
		return fmt.Errorf("a mapping key must be a string (at %s)", path)
	}
	child := key.Value
	if path != "" {
		child = path + "." + key.Value
	}
	if _, exists := out[key.Value]; exists {
		return fmt.Errorf("duplicate key %q at %s", key.Value, child)
	}
	v, err := d.convert(value, child)
	if err != nil {
		return err
	}
	out[key.Value] = v
	d.Positions[child] = Origin{File: d.File, Line: key.Line, Column: key.Column}
	return nil
}

// The tags a scalar is read as.
const (
	strTag   = "!!str"
	intTag   = "!!int"
	floatTag = "!!float"
	boolTag  = "!!bool"
	nullTag  = "!!null"
	// mergeTag is <<, which some readers take for a merge of another
	// mapping into this one.
	mergeTag = "!!merge"
)

// scalarTag is the type a scalar is read as: a string when it is quoted or
// a block, and otherwise what its text resolves to. A standard tag written
// on the value is not followed, so that !!int "8" and !!str 8 read as they
// are written rather than as they are tagged; a value with a tag of the
// site's own, !secret 8, is text.
//
// The configuration takes as numbers only what reads as one to every YAML
// reader, as the parser it was first read with did: a whole number in
// decimal, or after 0x, 0o or 0b, and a number with a decimal point. 1e3,
// 0X1F, a whole number too large for 64 bits and a date are text, which a
// field that takes a number refuses rather than reads as something else.
func scalarTag(n *yaml.Node) string {
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return strTag
	}
	tag := n.ShortTag()
	if n.Style&yaml.TaggedStyle != 0 {
		plain := yaml.Node{Kind: yaml.ScalarNode, Value: n.Value}
		tag = plain.ShortTag()
		if tag != nullTag && !strings.HasPrefix(n.Tag, "!!") && !strings.HasPrefix(n.Tag, "tag:yaml.org,2002:") {
			return strTag
		}
	}
	switch tag {
	case intTag:
		if digits := strings.TrimLeft(n.Value, "+-"); len(digits) > 1 && digits[0] == '0' && strings.ContainsRune("XOB", rune(digits[1])) {
			return strTag
		}
	case floatTag:
		if !strings.Contains(n.Value, ".") {
			return strTag
		}
	case nullTag, boolTag, strTag:
	default:
		return strTag
	}
	return tag
}

// scalarValue reads a scalar as a string, a whole number, a number, a
// boolean or nothing.
//
// A literal written with a leading zero is kept as a string. YAML
// implementations disagree about whether "0600" is six hundred or octal three
// hundred and eighty-four, and a file mode silently becoming 384 is the kind
// of bug that only shows up on the node. Fields that take a mode are declared
// as strings for the same reason.
func scalarValue(n *yaml.Node, path string) (any, error) {
	tag := scalarTag(n)
	lit := n.Value
	if tag == intTag || tag == floatTag {
		if digits := strings.TrimLeft(lit, "+-"); len(digits) > 1 && digits[0] == '0' && isDigits(digits) {
			return lit, nil
		}
	}
	switch tag {
	case nullTag:
		return nil, nil
	case boolTag:
		return strconv.ParseBool(lit)
	case intTag:
		plain := strings.ReplaceAll(lit, "_", "")
		if v, err := strconv.ParseInt(plain, 0, 64); err == nil {
			return v, nil
		}
		if v, err := strconv.ParseUint(plain, 0, 64); err == nil {
			return strconv.FormatUint(v, 10), nil
		}
		return nil, fmt.Errorf("the value at %s is not a number the configuration accepts", path)
	case floatTag:
		v, err := strconv.ParseFloat(strings.ReplaceAll(lit, "_", ""), 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, fmt.Errorf("the value at %s is not a number the configuration accepts", path)
		}
		return v, nil
	default:
		// Text, and what YAML would read as a time or binary data, which
		// the configuration takes as text.
		return lit, nil
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
