// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/invopop/jsonschema"
	validator "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
)

// schemaID is the base identifier the generated schemas are published under.
// Editors pick it up from the yaml-language-server comment in the example
// configuration.
const schemaID = "https://gsi-hpc.github.io/clusterctl/schema/v1alpha1/"

var (
	schemaOnce sync.Once
	schemas    map[string]*jsonschema.Schema
	compiled   map[string]*validator.Schema
	schemaErr  error
)

// kindTypes binds each kind to the Go type that defines it.
func kindTypes() map[string]any {
	return map[string]any{
		v1alpha1.KindConfig:        &v1alpha1.Config{},
		v1alpha1.KindSite:          &v1alpha1.Site{},
		v1alpha1.KindCluster:       &v1alpha1.Cluster{},
		v1alpha1.KindNodeInventory: &v1alpha1.NodeInventory{},
		v1alpha1.KindWorkstation:   &v1alpha1.Workstation{},
		v1alpha1.KindSecret:        &v1alpha1.Secret{},
	}
}

func buildSchemas() {
	schemas = map[string]*jsonschema.Schema{}
	compiled = map[string]*validator.Schema{}

	for kind, obj := range kindTypes() {
		r := &jsonschema.Reflector{
			// Unknown fields are rejected, so that a misspelled key is
			// reported instead of silently ignored.
			AllowAdditionalProperties: false,
			// Only fields tagged required are required.
			RequiredFromJSONSchemaTags: true,
			DoNotReference:             false,
			ExpandedStruct:             false,
		}
		s := r.Reflect(obj)
		s.ID = jsonschema.ID(schemaID + strings.ToLower(kind) + ".json")
		s.Title = "clusterctl " + kind
		schemas[kind] = s

		raw, err := json.Marshal(s)
		if err != nil {
			schemaErr = fmt.Errorf("generating the %s schema: %w", kind, err)
			return
		}
		doc, err := validator.UnmarshalJSON(strings.NewReader(string(raw)))
		if err != nil {
			schemaErr = fmt.Errorf("reading the %s schema: %w", kind, err)
			return
		}
		c := validator.NewCompiler()
		url := "schema://" + strings.ToLower(kind)
		if err := c.AddResource(url, doc); err != nil {
			schemaErr = fmt.Errorf("compiling the %s schema: %w", kind, err)
			return
		}
		sch, err := c.Compile(url)
		if err != nil {
			schemaErr = fmt.Errorf("compiling the %s schema: %w", kind, err)
			return
		}
		compiled[kind] = sch
	}
}

func loadSchemas() error {
	schemaOnce.Do(buildSchemas)
	return schemaErr
}

// SchemaJSON returns the JSON Schema of one kind, ready to be written to a
// file an editor can use.
func SchemaJSON(kind string) ([]byte, error) {
	if err := loadSchemas(); err != nil {
		return nil, err
	}
	s, ok := schemas[kind]
	if !ok {
		return nil, fmt.Errorf("unknown kind %q; expected one of %v", kind, v1alpha1.Kinds())
	}
	return json.MarshalIndent(s, "", "  ")
}

// SchemaKinds lists the kinds a schema can be produced for.
func SchemaKinds() []string { return v1alpha1.Kinds() }

// ValidateDocument checks a document against the schema of its kind and
// reports every problem with the position it was written at.
func ValidateDocument(doc *Document) error {
	if err := checkSopsDocument(doc); err != nil {
		return err
	}
	if err := v1alpha1.CheckTypeMeta(v1alpha1.TypeMeta{APIVersion: doc.APIVersion, Kind: doc.Kind}); err != nil {
		return fmt.Errorf("%s: %w", doc.Position("kind"), err)
	}
	if err := loadSchemas(); err != nil {
		return err
	}

	var problems []string
	// Unknown keys are checked first and separately, because the schema
	// knows the keys that were expected and can suggest the closest one.
	root := schemas[doc.Kind]
	problems = append(problems, unknownKeys(root, root, doc, doc.Data, "")...)

	if err := compiled[doc.Kind].Validate(toJSON(doc.Data)); err != nil {
		var verr *validator.ValidationError
		if errors.As(err, &verr) {
			problems = append(problems, describe(doc, verr)...)
		} else {
			problems = append(problems, err.Error())
		}
	}

	problems = append(problems, checkSecret(doc)...)

	problems = dedup(problems)
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%s is not valid:\n  %s", doc.File, strings.Join(problems, "\n  "))
}

// printer renders validation messages in English.
var printer = message.NewPrinter(language.English)

// describe turns a validation error tree into one message per leaf cause.
func describe(doc *Document, err *validator.ValidationError) []string {
	if len(err.Causes) == 0 {
		// Unknown keys are reported by unknownKeys, which can suggest the
		// field that was probably meant.
		if _, ok := err.ErrorKind.(*kind.AdditionalProperties); ok {
			return nil
		}
		path := instancePath(err.InstanceLocation)
		return []string{fmt.Sprintf("%s: %s: %s",
			doc.Position(path), displayPath(path), err.ErrorKind.LocalizedString(printer))}
	}
	var out []string
	for _, c := range err.Causes {
		out = append(out, describe(doc, c)...)
	}
	return out
}

// unknownKeys walks the document against the schema and reports every key the
// schema does not define, suggesting the closest one it does.
func unknownKeys(root, schema *jsonschema.Schema, doc *Document, value any, path string) []string {
	if schema == nil {
		return nil
	}
	resolved := resolveRef(root, schema)
	switch v := value.(type) {
	case map[string]any:
		var out []string
		for _, k := range slices.Sorted(maps.Keys(v)) {
			child := joinPath(path, k)
			if resolved.Properties != nil {
				if prop, ok := resolved.Properties.Get(k); ok {
					out = append(out, unknownKeys(root, prop, doc, v[k], child)...)
					continue
				}
			}
			// The key is not a declared field. Either the object is a map,
			// in which case additionalProperties describes its values, or
			// the key is a typo.
			add := resolved.AdditionalProperties
			switch {
			case add == nil:
				continue
			case isFalseSchema(add):
				out = append(out, fmt.Sprintf("%s: %s: unknown field %q%s",
					doc.Position(child), displayPath(child), k,
					suggest(k, propertyNames(resolved))))
			default:
				out = append(out, unknownKeys(root, add, doc, v[k], child)...)
			}
		}
		return out
	case []any:
		items := resolved.Items
		if items == nil {
			return nil
		}
		var out []string
		for i, e := range v {
			out = append(out, unknownKeys(root, items, doc, e, fmt.Sprintf("%s[%d]", path, i))...)
		}
		return out
	default:
		return nil
	}
}

// resolveRef follows a $ref into the definitions of the root schema.
func resolveRef(root, s *jsonschema.Schema) *jsonschema.Schema {
	for i := 0; s != nil && s.Ref != "" && i < 16; i++ {
		name := strings.TrimPrefix(s.Ref, "#/$defs/")
		next, ok := root.Definitions[name]
		if !ok {
			return s
		}
		s = next
	}
	return s
}

// isFalseSchema reports whether a subschema is the literal false, which is
// how "no other properties are allowed" is spelled.
func isFalseSchema(s *jsonschema.Schema) bool {
	if s == nil {
		return false
	}
	if s == jsonschema.FalseSchema {
		return true
	}
	data, err := json.Marshal(s)
	return err == nil && string(data) == "false"
}

func propertyNames(s *jsonschema.Schema) []string {
	if s.Properties == nil {
		return nil
	}
	var out []string
	for pair := s.Properties.Oldest(); pair != nil; pair = pair.Next() {
		out = append(out, pair.Key)
	}
	sort.Strings(out)
	return out
}

// suggest names the closest known field, when one is close enough to be a
// likely typo.
func suggest(got string, known []string) string {
	best, bestDist := "", 1<<30
	for _, k := range known {
		if d := editDistance(strings.ToLower(got), strings.ToLower(k)); d < bestDist {
			best, bestDist = k, d
		}
	}
	switch {
	case best != "" && bestDist <= 2:
		return fmt.Sprintf("; did you mean %q?", best)
	case len(known) > 0 && len(known) <= 12:
		return fmt.Sprintf("; expected one of %s", strings.Join(quoteAll(known), ", "))
	default:
		return ""
	}
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return out
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// instancePath turns a JSON pointer location into a dotted path.
func instancePath(loc []string) string {
	var b strings.Builder
	for _, part := range loc {
		if _, err := strconv.Atoi(part); err == nil {
			fmt.Fprintf(&b, "[%s]", part)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	return b.String()
}

// displayPath names the root of a document in messages.
func displayPath(path string) string {
	if path == "" {
		return "<document>"
	}
	return path
}

// toJSON converts a parsed tree into the value types the validator expects.
func toJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = toJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = toJSON(e)
		}
		return out
	case int64:
		return json.Number(strconv.FormatInt(t, 10))
	case int:
		return json.Number(strconv.Itoa(t))
	case uint64:
		return json.Number(strconv.FormatUint(t, 10))
	case float64:
		return json.Number(strconv.FormatFloat(t, 'g', -1, 64))
	default:
		return v
	}
}

func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
