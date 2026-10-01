// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config_test

import (
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/config"
)

// Whatever a file holds, reading it ends in documents or an error, and every
// key of a document that was read has the line it was written on.
func FuzzParseDocuments(f *testing.F) {
	for _, seed := range []string{
		"apiVersion: clusterctl/v1alpha1\nkind: Site\nspec: {fanout: {max: 16}}\n",
		"a:\n  - b: 0600\n    c: [1, 1e3, .5, !!str 8, !x y]\n---\n# c\n---\nd: |\n  text\n",
		"a: &x 1\nb: *x\n", "? [a]\n: 1\n", "\ufeffa: 0x1F\n", "a: [|\n x\n]\n", "...\n", "%YAML 1.2\n---\na: ~\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		docs, err := config.ParseDocuments("f.yaml", []byte(src))
		if err != nil {
			return
		}
		for _, doc := range docs {
			for key := range doc.Data {
				if doc.Position(key).Line < 1 {
					t.Fatalf("the key %q has no line:\n%q", key, src)
				}
			}
		}
	})
}
