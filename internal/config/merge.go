// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Tree is a configuration tree being built up layer by layer, together with
// the origin of every value in it.
type Tree struct {
	data    map[string]any
	origins map[string]Origin
}

// NewTree returns an empty tree.
func NewTree() *Tree {
	return &Tree{data: map[string]any{}, origins: map[string]Origin{}}
}

// Data returns the merged tree. The caller must not modify it.
func (t *Tree) Data() map[string]any { return t.data }

// Origin returns where the value at a dotted path came from.
func (t *Tree) Origin(path string) (Origin, bool) {
	o, ok := t.origins[path]
	return o, ok
}

// Origins returns every recorded path in sorted order.
func (t *Tree) Origins() map[string]Origin {
	out := make(map[string]Origin, len(t.origins))
	for k, v := range t.origins {
		out[k] = v
	}
	return out
}

// Paths returns the recorded paths in sorted order.
func (t *Tree) Paths() []string {
	out := make([]string, 0, len(t.origins))
	for k := range t.origins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MergeDocument merges the body of a document into the tree, attributing
// every value it sets to the given layer. Only the subtree named by prefix is
// taken; an empty prefix takes the whole document body.
//
// Mappings merge key by key. Sequences and scalars replace what was there,
// because a list of naming rules or of protected hosts only makes sense as a
// whole.
func (t *Tree) MergeDocument(layer string, doc *Document, from, into string) {
	t.MergeDocumentExcept(layer, doc, from, into)
}

// MergeDocumentExcept is MergeDocument without the named top level keys of
// the merged subtree.
//
// It keeps a document's own overrides table out of the merged tree: the
// overrides are applied by path, and mirroring them as data as well would
// show every override twice, once under its own path and once nested under
// the table it was written in.
func (t *Tree) MergeDocumentExcept(layer string, doc *Document, from, into string, except ...string) {
	value := lookup(doc.Data, from)
	if value == nil {
		return
	}
	if len(except) > 0 {
		if m, ok := value.(map[string]any); ok {
			filtered := make(map[string]any, len(m))
			for k, v := range m {
				if !contains(except, k) {
					filtered[k] = v
				}
			}
			value = filtered
		}
	}
	t.mergeValue(layer, doc, from, into, value)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// MergeMap merges a plain tree that did not come from a file.
func (t *Tree) MergeMap(layer string, value map[string]any) {
	t.mergeValue(layer, nil, "", "", value)
}

func (t *Tree) mergeValue(layer string, doc *Document, srcPath, dstPath string, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.mergeValue(layer, doc, joinPath(srcPath, k), joinPath(dstPath, k), v[k])
		}
	default:
		t.set(dstPath, value)
		t.origins[dstPath] = origin(layer, doc, srcPath)
		// A replaced subtree keeps no stale origins from earlier layers.
		prefix := dstPath + "."
		for p := range t.origins {
			if strings.HasPrefix(p, prefix) {
				delete(t.origins, p)
			}
		}
	}
}

func origin(layer string, doc *Document, path string) Origin {
	if doc == nil {
		return Origin{Layer: layer}
	}
	o := doc.Position(path)
	o.Layer = layer
	return o
}

// SetPath sets one dotted path, the way an override, an environment variable
// or --set does.
func (t *Tree) SetPath(layer, path string, value any, o Origin) error {
	if path == "" {
		return fmt.Errorf("an override needs a path")
	}
	if err := validatePath(path); err != nil {
		return err
	}
	t.set(path, value)
	o.Layer = layer
	t.origins[path] = o
	prefix := path + "."
	for p := range t.origins {
		if strings.HasPrefix(p, prefix) {
			delete(t.origins, p)
		}
	}
	return nil
}

// set writes value at a dotted path, creating the mappings on the way.
func (t *Tree) set(path string, value any) {
	if path == "" {
		if m, ok := value.(map[string]any); ok {
			t.data = m
		}
		return
	}
	parts := strings.Split(path, ".")
	node := t.data
	for _, p := range parts[:len(parts)-1] {
		next, ok := node[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[p] = next
		}
		node = next
	}
	node[parts[len(parts)-1]] = value
}

// lookup reads a dotted path out of a plain tree.
func lookup(data map[string]any, path string) any {
	if path == "" {
		return data
	}
	var current any = data
	for _, p := range strings.Split(path, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = m[p]
		if !ok {
			return nil
		}
	}
	return current
}

func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "." + b
}

// validatePath rejects override paths that cannot address anything.
func validatePath(path string) error {
	for _, p := range strings.Split(path, ".") {
		if p == "" {
			return fmt.Errorf("invalid path %q: it has an empty element", path)
		}
	}
	return nil
}

// ParseSetValue reads the right hand side of a --set assignment. The value is
// read as YAML, so that "30s" stays a string, "8" becomes a number and
// "[a,b]" becomes a list.
func ParseSetValue(raw string) (any, error) {
	docs, err := ParseDocuments("--set", []byte("value: "+raw))
	if err != nil {
		return nil, fmt.Errorf("invalid value %q: %w", raw, err)
	}
	if len(docs) == 0 {
		return nil, nil
	}
	return docs[0].Data["value"], nil
}

// FormatValue renders a merged value the way config view prints it.
func FormatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = FormatValue(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		// A structured value is rendered as compact JSON rather than as Go
		// syntax, so that it can be read and pasted back.
		encoded, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	default:
		return fmt.Sprint(v)
	}
}
