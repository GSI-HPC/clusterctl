// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"fmt"
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
	return info, nil
}

// DecryptSops decrypts a sops encrypted YAML file into memory and returns the
// plaintext YAML.
//
// The data key is recovered with the given age identities first, which are
// the ones workstation.identities names, and then with whatever sops itself
// finds: SOPS_AGE_KEY_FILE and its other variables, a PGP agent, or the
// credentials of a cloud key management service. The integrity of the whole
// file is verified before anything is returned.
func DecryptSops(data []byte, identities []age.Identity) ([]byte, error) {
	tree, err := loadSops(data)
	if err != nil {
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
	cipher := aes.NewCipher()
	mac, err := tree.Decrypt(key, cipher)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	stored, err := cipher.Decrypt(tree.Metadata.MessageAuthenticationCode, key,
		tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("reading the message authentication code: %w", err)
	}
	if stored != mac {
		return nil, fmt.Errorf("the file was changed without sops: its message authentication code does not match")
	}
	out, err := newSopsStore().EmitPlainFile(tree.Branches)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
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
