// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/secrets"
)

// sealedField is the key an inline secret is read from. A ciphertext written
// under any other key would be used as the literal text it is, so it is
// refused rather than silently passed to a backend.
const sealedField = "age"

// checkSecrets reports the inline secrets of a document that cannot work,
// without decrypting any of them: a damaged or truncated armor, a ciphertext
// under a key that is not read as one, a secret file that names both or
// neither of its sources and a recipient that is not a public key.
//
// It needs no identity, so "config validate" catches a bad paste on a
// machine that is not allowed to read the secret.
func checkSecrets(doc *Document) []string {
	var out []string
	walkStrings(doc.Data, "", func(path, key, value string) {
		if !secrets.IsArmored(value) {
			return
		}
		if key != sealedField {
			out = append(out, fmt.Sprintf("%s: %s: an age encrypted value is only read from a field named %q",
				doc.Position(path), displayPath(path), sealedField))
			return
		}
		if _, err := secrets.Inspect(value); err != nil {
			out = append(out, fmt.Sprintf("%s: %s: %v", doc.Position(path), displayPath(path), err))
		}
	})

	if doc.Kind != v1alpha1.KindSite {
		return out
	}
	spec, _ := doc.Data["spec"].(map[string]any)
	if recipients, ok := lookup(spec, "secrets.recipients").([]any); ok {
		for i, r := range recipients {
			s, _ := r.(string)
			if _, err := secrets.ParseRecipients([]string{s}); err != nil {
				path := fmt.Sprintf("spec.secrets.recipients[%d]", i)
				out = append(out, fmt.Sprintf("%s: %s: %v", doc.Position(path), path,
					strings.TrimPrefix(err.Error(), "recipient 1: ")))
			}
		}
	}
	if files, ok := lookup(spec, "services.cinc.secrets").([]any); ok {
		for i, f := range files {
			m, _ := f.(map[string]any)
			_, hasSource := m["source"]
			_, hasAge := m[sealedField]
			if hasSource == hasAge {
				path := fmt.Sprintf("spec.services.cinc.secrets[%d]", i)
				out = append(out, fmt.Sprintf("%s: %s: exactly one of source and age must be set",
					doc.Position(path), path))
			}
		}
	}
	return out
}

// walkStrings calls fn for every string in a tree with its dotted path and
// the key it was written under. For a dotted override key the last element
// is the key, so "credentials.bmc.password.age" counts as an age field.
func walkStrings(v any, path string, fn func(path, key, value string)) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := joinPath(path, k)
			if s, ok := t[k].(string); ok {
				fn(child, k[strings.LastIndex(k, ".")+1:], s)
				continue
			}
			walkStrings(t[k], child, fn)
		}
	case []any:
		for i, e := range t {
			child := fmt.Sprintf("%s[%d]", path, i)
			if s, ok := e.(string); ok {
				fn(child, "", s)
				continue
			}
			walkStrings(e, child, fn)
		}
	}
}
