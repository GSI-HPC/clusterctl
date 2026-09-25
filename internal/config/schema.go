// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
		s, sch, err := compileSchema(obj, "the "+kind+" schema", func(s *jsonschema.Schema) {
			s.ID = jsonschema.ID(schemaID + strings.ToLower(kind) + ".json")
			s.Title = "clusterctl " + kind
		})
		if err != nil {
			schemaErr = err
			return
		}
		schemas[kind], compiled[kind] = s, sch
	}
}

// compileSchema reflects the schema of a Go type, lets adjust change it, and
// compiles it for validation. Unknown fields are rejected, so that a
// misspelled key is reported instead of silently ignored, and only the
// fields tagged required are required.
func compileSchema(obj any, what string, adjust func(*jsonschema.Schema)) (*jsonschema.Schema, *validator.Schema, error) {
	r := &jsonschema.Reflector{AllowAdditionalProperties: false, RequiredFromJSONSchemaTags: true}
	s := r.Reflect(obj)
	adjust(s)
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, nil, fmt.Errorf("generating %s: %w", what, err)
	}
	doc, err := validator.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", what, err)
	}
	c := validator.NewCompiler()
	if err := c.AddResource("schema://clusterctl", doc); err != nil {
		return nil, nil, fmt.Errorf("compiling %s: %w", what, err)
	}
	sch, err := c.Compile("schema://clusterctl")
	if err != nil {
		return nil, nil, fmt.Errorf("compiling %s: %w", what, err)
	}
	return s, sch, nil
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

	problems := validate(schemas[doc.Kind], compiled[doc.Kind], doc)
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

// validate checks a document against a compiled schema, root being the
// schema it was compiled from, and returns one message per problem.
func validate(root *jsonschema.Schema, sch *validator.Schema, doc *Document) []string {
	err := sch.Validate(toJSON(doc.Data))
	var verr *validator.ValidationError
	if errors.As(err, &verr) {
		return describe(root, doc, verr)
	}
	if err != nil {
		return []string{err.Error()}
	}
	return nil
}

// describe turns a validation error tree into one message per leaf cause. An
// unknown key is reported with the field that was probably meant, which the
// schema the error points into knows.
func describe(root *jsonschema.Schema, doc *Document, err *validator.ValidationError) []string {
	if len(err.Causes) == 0 {
		path := instancePath(doc.Data, err.InstanceLocation)
		unknown, ok := err.ErrorKind.(*kind.AdditionalProperties)
		if !ok {
			return []string{fmt.Sprintf("%s: %s: %s",
				doc.Position(path), displayPath(path), err.ErrorKind.LocalizedString(printer))}
		}
		// Every object whose keys are checked is one of the definitions.
		_, name, _ := strings.Cut(err.SchemaURL, "#/$defs/")
		known := propertyNames(root.Definitions[name])
		var out []string
		for _, k := range unknown.Properties {
			child := joinPath(path, k)
			out = append(out, fmt.Sprintf("%s: %s: unknown field %q%s",
				doc.Position(child), displayPath(child), k, suggest(k, known)))
		}
		return out
	}
	var out []string
	for _, c := range err.Causes {
		out = append(out, describe(root, doc, c)...)
	}
	return out
}

func propertyNames(s *jsonschema.Schema) []string {
	if s == nil || s.Properties == nil {
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

// instancePath turns a JSON pointer location in data into the dotted path
// positions are recorded under: an index of a list in brackets, and a key of
// a mapping after a dot, even one that is a number.
func instancePath(data any, loc []string) string {
	path := ""
	for _, part := range loc {
		if list, ok := data.([]any); ok {
			path += "[" + part + "]"
			data = nil
			if i, err := strconv.Atoi(part); err == nil && i >= 0 && i < len(list) {
				data = list[i]
			}
			continue
		}
		m, _ := data.(map[string]any)
		data = m[part]
		path = joinPath(path, part)
	}
	return path
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
