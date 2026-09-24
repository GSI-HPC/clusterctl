// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/secrets"
)

// sopsKey is the top level key sops keeps its metadata under.
const sopsKey = "sops"

// secretSections are the parts of a Secret document that hold values. Only
// these are encrypted; the rest must stay readable without a key.
var secretSections = []string{"data", "binaryData"}

// encryptOnlyValues is the command the messages point at.
func encryptOnlyValues(file string) string {
	return "sops --encrypt --encrypted-regex '^(data|binaryData)$' --in-place " + filepath.Base(file)
}

// checkSopsDocument reports a document sops has touched that clusterctl
// cannot read, before the schema would report it less helpfully. It is run
// before anything else is checked, because an encrypted kind is not a kind.
func checkSopsDocument(doc *Document) error {
	_, encrypted := doc.Data[sopsKey]
	if !encrypted {
		return nil
	}
	for _, field := range []string{"apiVersion", "kind", "metadata.name"} {
		if s, ok := lookup(doc.Data, field).(string); ok && isSopsValue(s) {
			return fmt.Errorf("%s: %s is encrypted; clusterctl reads the kind and the name before it decrypts, so encrypt the values only: %s",
				doc.Position(field), field, encryptOnlyValues(doc.File))
		}
	}
	if doc.Kind != v1alpha1.KindSecret {
		return fmt.Errorf("%s: a %s document is encrypted with sops; only a Secret document may be, keep the values it needs there and refer to them with secretRef",
			doc.Position(sopsKey), doc.Kind)
	}
	return nil
}

// checkSecret reports the problems of a Secret document that do not need a
// key to find: a document that is not encrypted, a value added after it was
// encrypted, and a key given twice.
func checkSecret(doc *Document) []string {
	if doc.Kind != v1alpha1.KindSecret {
		return nil
	}
	var out []string
	if documentName(doc) == "" {
		out = append(out, fmt.Sprintf("%s: metadata.name: a Secret needs a name for secretRef to refer to", doc.Position("metadata")))
	}
	if _, ok := doc.Data[sopsKey]; !ok {
		out = append(out, fmt.Sprintf("%s: the Secret is not encrypted; encrypt it before it is committed: %s",
			doc.Position("kind"), encryptOnlyValues(doc.File)))
		return out
	}
	seen := map[string]string{}
	for _, section := range secretSections {
		values, _ := doc.Data[section].(map[string]any)
		for _, key := range sortedKeys(values) {
			path := section + "." + key
			if other, dup := seen[key]; dup {
				out = append(out, fmt.Sprintf("%s: %s: the key is also under %s; a key names one value", doc.Position(path), path, other))
			}
			seen[key] = section
			s, _ := values[key].(string)
			typ, ok := secrets.SopsValueType(s)
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("%s: %s: the value is not encrypted: it was added without sops; edit the file with \"sops %s\" instead",
					doc.Position(path), path, filepath.Base(doc.File)))
			case typ != "str":
				// sops parses the plaintext as this type, and the type is
				// not authenticated: anything but text is refused before a
				// key is used, so that a changed tag cannot put a value
				// into an error message.
				out = append(out, fmt.Sprintf("%s: %s: sops encrypted the value as type:%s, not as text, and would change it; "+
					"quote it in the plaintext, as in %s: \"0600\", and encrypt it again with \"sops %s\"",
					doc.Position(path), path, typ, key, filepath.Base(doc.File)))
			}
		}
	}
	return out
}

// checkSecretFile checks what only the whole file shows: that a Secret is
// alone in it, which is what sops expects of a file it encrypts, and that the
// sops metadata can be read.
func checkSecretFile(file string, raw []byte, docs []*Document) error {
	var secret *Document
	for _, doc := range docs {
		if doc.Kind == v1alpha1.KindSecret {
			secret = doc
		}
	}
	if secret == nil {
		return nil
	}
	if len(docs) > 1 {
		return fmt.Errorf("%s: a Secret document must be alone in its file; move the other %d documents out", file, len(docs)-1)
	}
	if _, ok := secret.Data[sopsKey]; !ok {
		return nil // reported by checkSecret, at a line
	}
	if _, err := secrets.InspectSops(raw); err != nil {
		return fmt.Errorf("%s: %w", secret.Position(sopsKey), err)
	}
	return nil
}

// isSopsValue reports whether a value is one sops encrypted.
func isSopsValue(s string) bool {
	return strings.HasPrefix(s, "ENC[") && strings.HasSuffix(s, "]")
}

// SecretKeys lists the keys a Secret document holds, which are readable
// without decrypting it.
func SecretKeys(doc *Document) []string {
	var out []string
	for _, section := range secretSections {
		values, _ := doc.Data[section].(map[string]any)
		out = append(out, sortedKeys(values)...)
	}
	sort.Strings(out)
	return out
}

// SecretSections are the top level mappings of a Secret document that hold
// its values, which is what to ask the decryption for.
func SecretSections() []string {
	return append([]string(nil), secretSections...)
}

// SecretValues turns the decrypted sections of a Secret document into its
// values by key, decoding binaryData. A value under data is used as written.
// No error quotes a value.
func SecretValues(file string, sections map[string]map[string]string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, section := range secretSections {
		values := sections[section]
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("%s: %s.%s: the key is also in another section; a key names one value", file, section, key)
			}
			text := values[key]
			if section == "binaryData" {
				decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
				if err != nil {
					return nil, fmt.Errorf("%s: binaryData.%s is not base64: %w", file, key, err)
				}
				out[key] = decoded
				continue
			}
			out[key] = []byte(text)
		}
	}
	return out, nil
}

// secretRefs lists every secretRef of the merged configuration together
// with what refers through it and the tree path its origin is recorded at.
func secretRefs(spec v1alpha1.EffectiveSpec) []namedRef {
	var out []namedRef
	for _, name := range sortedCredentials(spec.Credentials) {
		if ref := spec.Credentials[name].Password.SecretRef; ref != nil {
			out = append(out, namedRef{
				what: fmt.Sprintf("credential %q", name),
				path: "credentials." + name + ".password.secretRef.name",
				ref:  *ref,
			})
		}
	}
	for i, f := range spec.Services.Cinc.Secrets {
		if f.SecretRef != nil {
			out = append(out, namedRef{
				what: fmt.Sprintf("secret file %s", f.Target),
				path: "services.cinc.secrets",
				ref:  *f.SecretRef,
				item: i,
			})
		}
	}
	return out
}

type namedRef struct {
	what string
	path string
	ref  v1alpha1.SecretKeyRef
	item int
}

// checkSecretRefs reports every secretRef that names a Secret document or a
// key that does not exist, and every secret file that does not say where its
// content comes from. The keys of a Secret are readable without decrypting
// it, so a typo is caught before a key is ever needed.
func (r *Resolved) checkSecretRefs() error {
	var problems []string
	for i, f := range r.Spec.Services.Cinc.Secrets {
		if (f.Source == "") == (f.SecretRef == nil) {
			o, _ := r.Tree.Origin("services.cinc.secrets")
			problems = append(problems, fmt.Sprintf("%s: services.cinc.secrets[%d] (%s): exactly one of source and secretRef must be set",
				o, i, f.Target))
		}
	}
	for _, nr := range secretRefs(r.Spec) {
		o, _ := r.Tree.Origin(nr.path)
		doc, ok := r.Bundle.Secrets[nr.ref.Name]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %s refers to Secret %q, which no document defines (known: %v)",
				o, nr.what, nr.ref.Name, names(r.Bundle.Secrets)))
			continue
		}
		if !contains(SecretKeys(doc), nr.ref.Key) {
			problems = append(problems, fmt.Sprintf("%s: %s refers to key %q of Secret %q, which holds %v",
				o, nr.what, nr.ref.Key, nr.ref.Name, SecretKeys(doc)))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the configuration refers to secrets that do not exist:\n  %s", strings.Join(problems, "\n  "))
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCredentials(m map[string]v1alpha1.Credential) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
