// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

// sopsValue is how sops writes an encrypted value; it is the expression sops
// itself matches a value with.
var sopsValue = regexp.MustCompile(`^ENC\[AES256_GCM,data:(.+),iv:(.+),tag:(.+),type:(.+)\]`)

// SopsValueType returns the type sops recorded for an encrypted value, and
// whether the value is one sops encrypted at all.
//
// sops parses the plaintext as this type after decrypting it, but the type
// is outside what the encryption authenticates: anyone who can write the
// file can change it, and the parser's error then quotes the plaintext. Only
// "str" is read.
func SopsValueType(s string) (string, bool) {
	m := sopsValue.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[4], true
}

// SopsInfo describes a sops encrypted document, read without decrypting it.
type SopsInfo struct {
	// Keys are the master keys the data key is encrypted to, as sops names
	// their type ("age", "pgp", "kms", "gcp_kms", "azure_kv", "hc_vault",
	// "hckms") and the key itself, for example an age recipient or a key
	// ARN.
	Keys []SopsKey
	// Groups is the number of key groups, Threshold the number of them
	// needed to recover the data key when there are several.
	Groups    int
	Threshold int
	// LastModified is when sops last wrote the file.
	LastModified time.Time
	// EncryptedRegex is the rule that chose which values were encrypted.
	EncryptedRegex string
}

// SopsKey is one master key of a sops encrypted document.
type SopsKey struct {
	Type string
	ID   string
}

// Summary counts the master keys per type, for example "2 age, 1 pgp".
func (i SopsInfo) Summary() string {
	if len(i.Keys) == 0 {
		return "-"
	}
	counts := map[string]int{}
	for _, k := range i.Keys {
		counts[k.Type]++
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, t := range types {
		parts = append(parts, fmt.Sprintf("%d %s", counts[t], t))
	}
	out := strings.Join(parts, ", ")
	if i.Groups > 1 {
		out += fmt.Sprintf(" in %d groups, %d needed", i.Groups, i.Threshold)
	}
	return out
}

// ageKeyType is how sops names an age master key.
const ageKeyType = "age"

// DefaultSopsKeyTypes are the kinds of master key trusted when the
// workstation names none: age, which needs nothing but a local identity.
func DefaultSopsKeyTypes() []string { return []string{ageKeyType} }

// CheckKeyTypes refuses a document encrypted to a kind of master key that is
// not trusted here. The metadata that names the keys is not covered by the
// message authentication code, so anyone who can write the file can add a
// Vault or a key management service there, and sops would send this
// machine's credentials to it.
func (i SopsInfo) CheckKeyTypes(trusted []string) error {
	if len(trusted) == 0 {
		trusted = DefaultSopsKeyTypes()
	}
	var untrusted []string
	for _, k := range i.Keys {
		if !slices.Contains(trusted, k.Type) && !slices.Contains(untrusted, k.Type) {
			untrusted = append(untrusted, k.Type)
		}
	}
	if len(untrusted) == 0 {
		return nil
	}
	sort.Strings(untrusted)
	return fmt.Errorf("the file is encrypted to %s keys, which this workstation does not trust (it trusts %s); "+
		"add the kind to workstation.sopsKeyTypes if the site uses it",
		strings.Join(untrusted, " and "), strings.Join(trusted, ", "))
}

// sopsMetadata is the sops mapping of an encrypted YAML file, as sops
// v3.13.3 writes it (stores/stores.go). Only what clusterctl reports or
// checks is read; checkFields refuses a field it does not know, so that a
// kind of master key added to sops later cannot pass CheckKeyTypes unseen.
type sopsMetadata struct {
	Flat             sopsKeyGroup   `yaml:",inline"`
	KeyGroups        []sopsKeyGroup `yaml:"key_groups"`
	ShamirThreshold  int            `yaml:"shamir_threshold"`
	LastModified     string         `yaml:"lastmodified"`
	MAC              string         `yaml:"mac"`
	EncryptedRegex   string         `yaml:"encrypted_regex"`
	MACOnlyEncrypted bool           `yaml:"mac_only_encrypted"`
}

// sopsKeyGroup lists the master keys of one group, by type.
type sopsKeyGroup struct {
	KMS []struct {
		ARN  string `yaml:"arn"`
		Role string `yaml:"role"`
	} `yaml:"kms"`
	GCPKMS []struct {
		ResourceID string `yaml:"resource_id"`
	} `yaml:"gcp_kms"`
	HCKMS []struct {
		KeyID string `yaml:"key_id"`
	} `yaml:"hckms"`
	AzureKV []struct {
		VaultURL string `yaml:"vault_url"`
		Name     string `yaml:"name"`
		Version  string `yaml:"version"`
	} `yaml:"azure_kv"`
	Vault []struct {
		Address    string `yaml:"vault_address"`
		EnginePath string `yaml:"engine_path"`
		KeyName    string `yaml:"key_name"`
	} `yaml:"hc_vault"`
	PGP []struct {
		Fingerprint string `yaml:"fp"`
	} `yaml:"pgp"`
	Age []struct {
		Recipient string `yaml:"recipient"`
	} `yaml:"age"`
}

// keys lists the master keys of a group in the order sops reads them into
// one (stores.internalGroupFrom), named the way sops names them.
func (g sopsKeyGroup) keys() []SopsKey {
	var out []SopsKey
	for _, k := range g.KMS {
		id := k.ARN
		if k.Role != "" {
			id += "+" + k.Role
		}
		out = append(out, SopsKey{"kms", id})
	}
	for _, k := range g.GCPKMS {
		out = append(out, SopsKey{"gcp_kms", k.ResourceID})
	}
	for _, k := range g.HCKMS {
		out = append(out, SopsKey{"hckms", k.KeyID})
	}
	for _, k := range g.AzureKV {
		out = append(out, SopsKey{"azure_kv", k.VaultURL + "/keys/" + k.Name + "/" + k.Version})
	}
	for _, k := range g.Vault {
		out = append(out, SopsKey{"hc_vault", k.Address + "/v1/" + k.EnginePath + "/keys/" + k.KeyName})
	}
	for _, k := range g.PGP {
		out = append(out, SopsKey{"pgp", k.Fingerprint})
	}
	for _, k := range g.Age {
		out = append(out, SopsKey{ageKeyType, k.Recipient})
	}
	return out
}

// keyTypes are the fields of a key group, one per kind of master key.
var keyTypes = []string{"kms", "gcp_kms", "hckms", "azure_kv", "hc_vault", "pgp", ageKeyType}

// metadataFields are the other fields sops writes into its mapping.
var metadataFields = []string{
	"key_groups", "shamir_threshold", "lastmodified", "mac", "version", "mac_only_encrypted",
	"unencrypted_suffix", "encrypted_suffix", "unencrypted_regex", "encrypted_regex",
	"unencrypted_comment_regex", "encrypted_comment_regex",
}

// sopsDocument is a sops encrypted YAML file, read without decrypting it.
type sopsDocument struct {
	info SopsInfo
	// values is the document without its sops mapping.
	values map[string]any
}

// InspectSops reads the sops metadata of a YAML file without decrypting
// anything, so that a damaged or hand edited file is reported when the
// configuration loads rather than when a node is half way through a
// reinstall. It needs neither a key nor sops.
func InspectSops(data []byte) (SopsInfo, error) {
	doc, err := readSops(data)
	if err != nil {
		return SopsInfo{}, err
	}
	return doc.info, nil
}

func readSops(data []byte) (sopsDocument, error) {
	file, err := parser.ParseBytes(data, 0)
	if err != nil {
		return sopsDocument{}, fmt.Errorf("reading the sops metadata: %w", err)
	}
	docs := 0
	for _, d := range file.Docs {
		if d.Body != nil {
			docs++
		}
	}
	if docs != 1 {
		return sopsDocument{}, fmt.Errorf("reading the sops metadata: the file holds %d documents, want 1", docs)
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return sopsDocument{}, fmt.Errorf("reading the sops metadata: %w", err)
	}
	raw, ok := values["sops"].(map[string]any)
	if !ok {
		return sopsDocument{}, fmt.Errorf("reading the sops metadata: the file has no sops mapping; it is not encrypted")
	}
	delete(values, "sops")
	var meta struct {
		Sops sopsMetadata `yaml:"sops"`
	}
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return sopsDocument{}, fmt.Errorf("reading the sops metadata: %w", err)
	}
	info, err := inspect(meta.Sops)
	if err != nil {
		return sopsDocument{}, err
	}
	if err := checkFields(raw); err != nil {
		return sopsDocument{}, err
	}
	return sopsDocument{info: info, values: values}, nil
}

// checkFields refuses a field of the sops mapping, or of one of its key
// groups, that clusterctl does not know. The sops that decrypts can be
// newer than clusterctl, and a kind of master key clusterctl does not list
// is one CheckKeyTypes could not refuse.
func checkFields(raw map[string]any) error {
	for field, value := range raw {
		if !slices.Contains(keyTypes, field) && !slices.Contains(metadataFields, field) {
			return fmt.Errorf("the sops metadata has a field clusterctl does not know, %q; "+
				"a kind of master key it cannot check is refused", field)
		}
		if field != "key_groups" {
			continue
		}
		groups, _ := value.([]any)
		for i, group := range groups {
			g, _ := group.(map[string]any)
			for f := range g {
				if !slices.Contains(keyTypes, f) {
					return fmt.Errorf("the sops metadata has a field clusterctl does not know, %q in key group %d; "+
						"a kind of master key it cannot check is refused", f, i+1)
				}
			}
		}
	}
	return nil
}

func inspect(m sopsMetadata) (SopsInfo, error) {
	info := SopsInfo{
		Threshold:      m.ShamirThreshold,
		EncryptedRegex: m.EncryptedRegex,
	}
	// sops reads the keys at the top of the mapping as one group and then
	// ignores key_groups; a file that has both reads differently to
	// whoever looks only at one of them.
	flat := m.Flat.keys()
	switch {
	case len(flat) > 0 && len(m.KeyGroups) > 0:
		return SopsInfo{}, fmt.Errorf("the sops metadata names master keys both in key_groups and outside it; sops would ignore key_groups")
	case len(flat) > 0:
		info.Groups = 1
		info.Keys = flat
	default:
		info.Groups = len(m.KeyGroups)
		for _, g := range m.KeyGroups {
			info.Keys = append(info.Keys, g.keys()...)
		}
	}
	if len(info.Keys) == 0 {
		return SopsInfo{}, fmt.Errorf("the sops metadata names no key to decrypt with")
	}
	if m.MAC == "" {
		return SopsInfo{}, fmt.Errorf("the sops metadata has no message authentication code")
	}
	lastModified, err := time.Parse(time.RFC3339, m.LastModified)
	if err != nil {
		return SopsInfo{}, fmt.Errorf("the sops metadata has no readable lastmodified time")
	}
	info.LastModified = lastModified
	if m.MACOnlyEncrypted {
		return SopsInfo{}, fmt.Errorf("the sops metadata sets mac_only_encrypted, under which the kind and the name can be changed without a key; " +
			"encrypt the file again without --mac-only-encrypted")
	}
	return info, nil
}

// checkValueTypes refuses an encrypted value sops would parse as anything
// but a string, before sops is run: sops parses the plaintext as the
// unauthenticated type tag says, and its parser's error quotes the value.
func checkValueTypes(values map[string]any) error {
	var bad error
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case string:
			if typ, ok := SopsValueType(v); ok && typ != "str" && bad == nil {
				bad = fmt.Errorf("%s: the value was encrypted as type:%s; only text (type:str) is read", path, typ)
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(joinPath(path, k), v[k])
			}
		case []any:
			for i, e := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk("", values)
	return bad
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
