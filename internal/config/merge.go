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

// Value returns the merged value at a dotted path, nil when nothing is
// there. A key with a dot in it, such as the group rack.R01, is found as
// well: at each mapping the longest run of path elements that names a key
// is tried first.
func (t *Tree) Value(path string) any {
	return lookupKeys(t.data, splitPath(path))
}

func lookupKeys(value any, keys []string) any {
	if len(keys) == 0 {
		return value
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for n := len(keys); n > 0; n-- {
		child, ok := m[strings.Join(keys[:n], ".")]
		if !ok {
			continue
		}
		if found := lookupKeys(child, keys[n:]); found != nil || n == len(keys) {
			return found
		}
	}
	return nil
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
	t.mergeValue(layer, docOrigin(doc), from, splitPath(into), value)
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
	t.mergeValue(layer, fixedOrigin(Origin{}), "", nil, value)
}

// originFunc says where the value written at a path of its source was
// written.
type originFunc func(srcPath string) Origin

// docOrigin finds values in a document.
func docOrigin(doc *Document) originFunc {
	return func(path string) Origin { return doc.Position(path) }
}

// fixedOrigin attributes every value to one place, for values that did not
// come from a document.
func fixedOrigin(o Origin) originFunc {
	return func(string) Origin { return o }
}

// mergeValue merges value at dst, the path split into its keys. The keys are
// kept apart rather than joined and split again, so that a key with a dot in
// it, a group called rack.R01, stays one key.
func (t *Tree) mergeValue(layer string, from originFunc, srcPath string, dst []string, value any) {
	if m, ok := value.(map[string]any); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.mergeValue(layer, from, joinPath(srcPath, k), append(dst[:len(dst):len(dst)], k), m[k])
		}
		return
	}
	t.set(dst, value)
	path := strings.Join(dst, ".")
	o := from(srcPath)
	o.Layer = layer
	// A replaced subtree keeps no stale origins from earlier layers, and a
	// value that replaced a whole section earlier no longer speaks for it.
	prefix := path + "."
	for p := range t.origins {
		if strings.HasPrefix(p, prefix) || strings.HasPrefix(path, p+".") {
			delete(t.origins, p)
		}
	}
	t.origins[path] = o
}

// SetPath applies one override, the way an overrides table, an environment
// variable or --set does. The path is dotted; the value is checked against
// the schema of the merged configuration before anything is changed.
//
// A mapping value merges key by key, the way a mapping in a document does:
// an override written as safety: {confirmAbove: 4} changes confirmAbove and
// leaves the protected hosts alone.
func (t *Tree) SetPath(layer, path string, value any, o Origin) error {
	return t.override(layer, path, value, o, fixedOrigin(o), "")
}

// override applies one override whose value came from from, at srcPath of
// its source. at is where the override was written: every problem is
// reported there, so the error it returns needs no position added.
func (t *Tree) override(layer, path string, value any, at Origin, from originFunc, srcPath string) error {
	// A problem is reported where the override was written, not with the
	// layer it would have been merged into.
	at.Layer = ""
	if path == "" {
		return fmt.Errorf("%s: an override needs a path", at)
	}
	if err := validatePath(path); err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	if problems := checkOverride(path, value, at); len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n  "))
	}
	t.mergeValue(layer, from, srcPath, splitPath(path), value)
	return nil
}

// set writes value at a path, creating the mappings on the way.
func (t *Tree) set(path []string, value any) {
	if len(path) == 0 {
		if m, ok := value.(map[string]any); ok {
			t.data = m
		}
		return
	}
	node := t.data
	for _, p := range path[:len(path)-1] {
		next, ok := node[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[p] = next
		}
		node = next
	}
	node[path[len(path)-1]] = value
}

// splitPath splits a dotted path into its keys.
func splitPath(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(path, ".")
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
