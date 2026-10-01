// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// oldMergeValue is mergeValue as it was, which held every leaf against
// every origin.
func (t *Tree) oldMergeValue(layer string, from originFunc, srcPath string, dst []string, value any) {
	if m, ok := value.(map[string]any); ok {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			t.oldMergeValue(layer, from, joinPath(srcPath, k), append(dst[:len(dst):len(dst)], k), m[k])
		}
		return
	}
	t.set(dst, value)
	path := strings.Join(dst, ".")
	o := from(srcPath)
	o.Layer = layer
	for p, keys := range t.keys {
		if isPrefix(dst, keys) || isPrefix(keys, dst) {
			delete(t.origins, p)
			delete(t.keys, p)
		}
	}
	t.origins[path] = o
	t.keys[path] = slices.Clone(dst)
}

// mergeValue held every leaf against every origin, which a configuration
// of 2,000 static groups took 47 ms over; it drops the origins only where a
// value replaced a section or a leaf on its way. Layers of random values,
// sections and leaves over each other's paths, leave the same values and
// the same origins as the old way did.
func TestMergeKeepsTheOriginsItKept(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	keys := []string{"a", "b", "c", "d.e"}
	var value func(depth int) any
	value = func(depth int) any {
		switch n := rng.IntN(6); {
		case depth > 2 || n < 2:
			return fmt.Sprint(rng.IntN(100))
		case n == 2:
			return []any{rng.IntN(3)}
		case n == 3:
			return map[string]any{}
		default:
			m := map[string]any{}
			for range 1 + rng.IntN(3) {
				m[keys[rng.IntN(len(keys))]] = value(depth + 1)
			}
			return m
		}
	}
	for round := range 200 {
		now, then := NewTree(), NewTree()
		for layer := range 8 {
			var dst []string
			for range rng.IntN(3) {
				dst = append(dst, keys[rng.IntN(len(keys))])
			}
			v := value(len(dst))
			if len(dst) == 0 {
				if _, ok := v.(map[string]any); !ok {
					v = map[string]any{keys[0]: v}
				}
			}
			name := fmt.Sprint("layer", layer)
			from := fixedOrigin(Origin{File: name})
			now.mergeValue(name, from, "", dst, v)
			then.oldMergeValue(name, from, "", dst, v)
		}
		if !reflect.DeepEqual(now.data, then.data) {
			t.Fatalf("round %d: data %v, want %v", round, now.data, then.data)
		}
		if !reflect.DeepEqual(now.origins, then.origins) || !reflect.DeepEqual(now.keys, then.keys) {
			t.Fatalf("round %d: origins %v, want %v", round, now.origins, then.origins)
		}
	}
}
