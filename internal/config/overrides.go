// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/invopop/jsonschema"
	validator "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
)

// An override addresses the merged configuration by a dotted path, and its
// table is typed as a plain map, so the schema of the kind it is written in
// cannot check it. It is checked here instead, against the schema of the
// merged configuration: the path key by key and with the case it was written
// in, and the value with the type the path expects. Without that, a key such
// as safety.protectedhosts would be matched by the case insensitive JSON
// decoder, and a value such as safety: null would erase a whole section.

var (
	effectiveOnce     sync.Once
	effectiveSchema   *jsonschema.Schema
	effectiveCompiled *validator.Schema
	effectiveErr      error
)

func buildEffectiveSchema() {
	effectiveSchema, effectiveCompiled, effectiveErr = compileSchema(&v1alpha1.EffectiveSpec{},
		"the schema of the merged configuration", func(s *jsonschema.Schema) {
			// An override sets part of a value that the layers before it
			// complete, so no field of it is required on its own. The
			// merged result is checked as a whole when it is decoded.
			s.Required = nil
			for _, def := range s.Definitions {
				def.Required = nil
			}
		})
}

// checkOverride checks one override against the schema of the merged
// configuration and returns its problems, each reported at, the place the
// override was written.
func checkOverride(path string, value any, at Origin) []string {
	effectiveOnce.Do(buildEffectiveSchema)
	if effectiveErr != nil {
		return []string{effectiveErr.Error()}
	}

	// The override is checked as the tree it would be merged as, a
	// document of its own whose every value was written where the
	// override was.
	keys := splitPath(path)
	tree := value
	for i := len(keys) - 1; i >= 0; i-- {
		tree = map[string]any{keys[i]: tree}
	}
	at.Layer = ""
	doc := &Document{File: at.File, Data: tree.(map[string]any), Positions: map[string]Origin{keys[0]: at}}

	problems := validate(effectiveSchema, effectiveCompiled, doc)
	problems = dedup(problems)
	sort.Strings(problems)
	return problems
}

// checkDocumentOverrides checks every override table of a document, those of
// every context of a Config document included, and not only those of the
// context that happens to be selected.
func checkDocumentOverrides(doc *Document) error {
	var problems []string
	check := func(value any, at string) {
		overrides, ok := value.(map[string]any)
		if !ok {
			return
		}
		for _, k := range slices.Sorted(maps.Keys(overrides)) {
			where := at + "." + k
			if err := validatePath(k); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s: %v", doc.Position(where), where, err))
				continue
			}
			problems = append(problems, checkOverride(k, overrides[k], doc.Position(where))...)
		}
	}
	switch doc.Kind {
	case v1alpha1.KindConfig:
		contexts, _ := doc.Data["contexts"].([]any)
		for i, ctx := range contexts {
			if m, ok := ctx.(map[string]any); ok {
				check(m["overrides"], fmt.Sprintf("contexts[%d].overrides", i))
			}
		}
	case v1alpha1.KindCluster, v1alpha1.KindWorkstation:
		check(lookup(doc.Data, "spec.overrides"), "spec.overrides")
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s is not valid:\n  %s", doc.File, strings.Join(problems, "\n  "))
}
