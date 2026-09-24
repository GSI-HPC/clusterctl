// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
	sopsyaml "github.com/getsops/sops/v3/stores/yaml"
	"google.golang.org/grpc"
)

// errUnreadable is all that is said about a file whose data key opened but
// whose values did not: what sops says then can quote a decrypted value.
var errUnreadable = errors.New("the file could not be decrypted or was changed without sops")

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
	// their type ("age", "pgp", "kms", "gcp_kms", "azure_kv", "hc_vault")
	// and the key itself, for example an age recipient or a key ARN.
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

// InspectSops reads the sops metadata of a YAML file without decrypting
// anything, so that a damaged or hand edited file is reported when the
// configuration loads rather than when a node is half way through a
// reinstall.
func InspectSops(data []byte) (SopsInfo, error) {
	tree, err := loadSops(data)
	if err != nil {
		return SopsInfo{}, err
	}
	m := tree.Metadata
	info := SopsInfo{
		Groups:         len(m.KeyGroups),
		Threshold:      m.ShamirThreshold,
		LastModified:   m.LastModified,
		EncryptedRegex: m.EncryptedRegex,
	}
	for _, group := range m.KeyGroups {
		for _, k := range group {
			info.Keys = append(info.Keys, SopsKey{Type: k.TypeToIdentifier(), ID: k.ToString()})
		}
	}
	if len(info.Keys) == 0 {
		return SopsInfo{}, fmt.Errorf("the sops metadata names no key to decrypt with")
	}
	if m.MessageAuthenticationCode == "" {
		return SopsInfo{}, fmt.Errorf("the sops metadata has no message authentication code")
	}
	if err := checkMetadata(m); err != nil {
		return SopsInfo{}, err
	}
	return info, nil
}

// checkMetadata refuses the settings of sops under which the message
// authentication code does not cover the whole file.
func checkMetadata(m sops.Metadata) error {
	if m.MACOnlyEncrypted {
		return fmt.Errorf("the sops metadata sets mac_only_encrypted, under which the kind and the name can be changed without a key; " +
			"encrypt the file again without --mac-only-encrypted")
	}
	return nil
}

// DecryptSops decrypts a sops encrypted YAML file into memory and returns
// the values of the given top level mappings, by key.
//
// The data key is recovered with the given age identities first, which are
// the ones workstation.identities names, and then with whatever sops itself
// finds: SOPS_AGE_KEY_FILE and its other variables, a PGP agent, or the
// credentials of a cloud key management service. The integrity of the whole
// file is verified before anything is returned.
//
// The values are read from the decrypted tree as sops holds it, never
// written out and parsed again, and no error says anything of them.
func DecryptSops(data []byte, identities []age.Identity, sections []string) (values map[string]map[string]string, err error) {
	tree, err := loadSops(data)
	if err != nil {
		return nil, err
	}
	if err := checkMetadata(tree.Metadata); err != nil {
		return nil, err
	}
	if err := checkValueTypes(tree.Branches); err != nil {
		return nil, err
	}
	var attempts attempts
	services := []keyservice.KeyServiceClient{}
	if len(identities) > 0 {
		services = append(services, recording{ageKeyService{identities: identities}, "workstation.identities", &attempts})
	}
	services = append(services, recording{keyservice.NewLocalClient(), "sops", &attempts})

	key, err := tree.Metadata.GetDataKeyWithKeyServices(services, nil)
	if err != nil {
		// The error of sops only counts the key groups that failed; what
		// was tried and why it failed is what an administrator can act on.
		return nil, fmt.Errorf("no key available here opens it: %s", attempts)
	}

	// From here on an error of sops may carry plaintext, and a panic in it
	// is a file it could not read.
	defer func() {
		if recover() != nil {
			values, err = nil, errUnreadable
		}
	}()
	cipher := aes.NewCipher()
	mac, err := tree.Decrypt(key, cipher)
	if err != nil {
		return nil, errUnreadable
	}
	stored, err := cipher.Decrypt(tree.Metadata.MessageAuthenticationCode, key,
		tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil || stored != mac {
		return nil, errUnreadable
	}
	return sectionValues(tree.Branches, sections)
}

// checkValueTypes refuses an encrypted value sops would parse as anything
// but a string, before anything is decrypted.
func checkValueTypes(branches sops.TreeBranches) error {
	var bad error
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case string:
			if typ, ok := SopsValueType(v); ok && typ != "str" && bad == nil {
				bad = fmt.Errorf("%s: the value was encrypted as type:%s; only text (type:str) is read", path, typ)
			}
		case sops.TreeBranch:
			for _, item := range v {
				if _, comment := item.Key.(sops.Comment); !comment {
					walk(joinPath(path, fmt.Sprint(item.Key)), item.Value)
				}
			}
		case []any:
			for i, e := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	for _, branch := range branches {
		walk("", branch)
	}
	return bad
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// sectionValues reads the strings of the named top level mappings out of a
// decrypted tree. An error names a key, never a value.
func sectionValues(branches sops.TreeBranches, sections []string) (map[string]map[string]string, error) {
	if len(branches) != 1 {
		return nil, fmt.Errorf("the file holds %d documents, want 1", len(branches))
	}
	out := map[string]map[string]string{}
	for _, item := range branches[0] {
		section, ok := item.Key.(string)
		if !ok || !slices.Contains(sections, section) {
			continue
		}
		if _, dup := out[section]; dup {
			return nil, fmt.Errorf("%s is given twice", section)
		}
		values := map[string]string{}
		switch branch := item.Value.(type) {
		case nil:
		case sops.TreeBranch:
			for _, e := range branch {
				if _, comment := e.Key.(sops.Comment); comment {
					continue
				}
				key, ok := e.Key.(string)
				if !ok {
					return nil, fmt.Errorf("%s: the key %v is not a string", section, e.Key)
				}
				if _, dup := values[key]; dup {
					return nil, fmt.Errorf("%s.%s: the key is given twice", section, key)
				}
				value, ok := e.Value.(string)
				if !ok {
					return nil, fmt.Errorf("%s.%s: a value must be a string", section, key)
				}
				values[key] = value
			}
		default:
			return nil, fmt.Errorf("%s: must be a mapping of keys to values", section)
		}
		out[section] = values
	}
	return out, nil
}

func newSopsStore() *sopsyaml.Store {
	return sopsyaml.NewStore(&config.YAMLStoreConfig{})
}

func loadSops(data []byte) (sops.Tree, error) {
	tree, err := newSopsStore().LoadEncryptedFile(data)
	if err != nil {
		return sops.Tree{}, fmt.Errorf("reading the sops metadata: %w", err)
	}
	return tree, nil
}

// errNotAge is how ageKeyService passes on a key that is not its to open.
var errNotAge = errors.New("not an age key")

// attempts records each master key a key service failed to open.
type attempts []string

func (a attempts) String() string {
	if len(a) == 0 {
		return "no key service was tried"
	}
	return strings.Join(a, "; ")
}

// recording is a key service that notes each failure of the one it wraps.
type recording struct {
	keyservice.KeyServiceClient
	via string
	log *attempts
}

func (r recording) Decrypt(ctx context.Context, req *keyservice.DecryptRequest, opts ...grpc.CallOption) (*keyservice.DecryptResponse, error) {
	resp, err := r.KeyServiceClient.Decrypt(ctx, req, opts...)
	if err != nil && !errors.Is(err, errNotAge) {
		*r.log = append(*r.log, fmt.Sprintf("%s via %s: %v", keyName(req.GetKey()), r.via, err))
	}
	return resp, err
}

// keyName says which master key a request is for.
func keyName(k *keyservice.Key) string {
	switch t := k.GetKeyType().(type) {
	case *keyservice.Key_AgeKey:
		return "age " + t.AgeKey.GetRecipient()
	case *keyservice.Key_PgpKey:
		return "pgp " + t.PgpKey.GetFingerprint()
	case *keyservice.Key_KmsKey:
		return "kms " + t.KmsKey.GetArn()
	case *keyservice.Key_GcpKmsKey:
		return "gcp_kms " + t.GcpKmsKey.GetResourceId()
	case *keyservice.Key_AzureKeyvaultKey:
		return "azure_kv " + t.AzureKeyvaultKey.GetVaultUrl() + "/" + t.AzureKeyvaultKey.GetName()
	case *keyservice.Key_VaultKey:
		return "hc_vault " + t.VaultKey.GetVaultAddress() + "/" + t.VaultKey.GetKeyName()
	case *keyservice.Key_HckmsKey:
		return "hckms " + t.HckmsKey.GetKeyId()
	default:
		return fmt.Sprintf("%T", t)
	}
}

// ageKeyService recovers a data key encrypted to an age recipient with the
// identities clusterctl was given, so that the keys named by
// workstation.identities, OpenSSH keys among them, open sops files as they
// open every other secret.
type ageKeyService struct {
	identities []age.Identity
}

func (s ageKeyService) Decrypt(_ context.Context, req *keyservice.DecryptRequest, _ ...grpc.CallOption) (*keyservice.DecryptResponse, error) {
	ak := req.GetKey().GetAgeKey()
	if ak == nil {
		return nil, errNotAge
	}
	mk := &sopsage.MasterKey{Recipient: ak.GetRecipient(), EncryptedKey: string(req.GetCiphertext())}
	sopsage.ParsedIdentities(s.identities).ApplyToMasterKey(mk)
	plain, err := mk.Decrypt()
	if err != nil {
		return nil, err
	}
	return &keyservice.DecryptResponse{Plaintext: plain}, nil
}

func (ageKeyService) Encrypt(context.Context, *keyservice.EncryptRequest, ...grpc.CallOption) (*keyservice.EncryptResponse, error) {
	return nil, fmt.Errorf("clusterctl does not encrypt; use sops")
}
