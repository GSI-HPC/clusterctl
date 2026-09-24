// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
)

const plainSecret = `apiVersion: clusterctl/v1alpha1
kind: Secret
metadata:
  name: vault
data:
  bmc-password: hunter2
binaryData:
  munge-key: bXVuZ2U=
`

// sealed encrypts a plaintext document to a fresh age key.
func sealed(t *testing.T, plaintext string) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return string(sopstest.Encrypt(t, plaintext, id.Recipient().String()))
}

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSecretDocumentIsIndexedWithoutAKey(t *testing.T) {
	dir := writeFiles(t, map[string]string{"secrets.sops.yaml": sealed(t, plainSecret)})
	b, err := config.Load([]string{filepath.Join(dir, "secrets.sops.yaml")})
	if err != nil {
		t.Fatalf("an encrypted Secret was rejected: %v", err)
	}
	doc, ok := b.Secrets["vault"]
	if !ok {
		t.Fatal("the Secret was not indexed by name")
	}
	if got, want := config.SecretKeys(doc), []string{"bmc-password", "munge-key"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SecretKeys = %v, want %v", got, want)
	}
}

func TestSecretDocumentMistakesAreReportedWhereWritten(t *testing.T) {
	good := sealed(t, plainSecret)
	tests := []struct {
		name string
		file string
		want []string
	}{
		{
			name: "not encrypted",
			file: plainSecret,
			want: []string{"secrets.sops.yaml:2:1", "not encrypted", "--encrypted-regex '^(data|binaryData)$'", "secrets.sops.yaml"},
		},
		{
			name: "value added without sops",
			file: strings.Replace(good, "\ndata:\n", "\ndata:\n    pdu-password: sneaky\n", 1),
			want: []string{"data.pdu-password", "added without sops", `"sops secrets.sops.yaml"`},
		},
		{
			name: "everything encrypted",
			file: string(sopstest.EncryptWithRegex(t, plainSecret, "", mustRecipient(t))),
			want: []string{"apiVersion is encrypted", "--encrypted-regex"},
		},
		{
			name: "not alone in its file",
			file: good + "---\napiVersion: clusterctl/v1alpha1\nkind: Config\n",
			want: []string{"must be alone in its file"},
		},
		{
			name: "no name",
			file: sealed(t, strings.Replace(plainSecret, "metadata:\n  name: vault\n", "metadata: {}\n", 1)),
			want: []string{"a Secret needs a name"},
		},
		{
			name: "key in both sections",
			file: sealed(t, plainSecret+"  bmc-password: aGk=\n"),
			want: []string{"binaryData.bmc-password", "also under data"},
		},
		{
			// sops wrote an unquoted 0600 as the integer 384 (report
			// section 8.6); a type tag changed by hand is refused the
			// same way (8.1).
			name: "value sops would retype",
			file: sealed(t, plainSecret+"  pin: 0600\n"),
			want: []string{"binaryData.pin", "type:int", "quote it"},
		},
		{
			name: "type tag changed without a key",
			file: strings.Replace(good, ",type:str]\n", ",type:bytes]\n", 1),
			want: []string{"data.bmc-password", "type:bytes"},
		},
		{
			name: "MAC over the encrypted values only",
			file: string(sopstest.EncryptWith(t, plainSecret,
				sopstest.Options{Regex: sopstest.EncryptedRegex, MACOnlyEncrypted: true}, mustRecipient(t))),
			want: []string{"secrets.sops.yaml", "mac_only_encrypted"},
		},
		{
			name: "sops metadata damaged",
			file: strings.Replace(good, "    mac: ENC[", "    notmac: ENC[", 1),
			want: []string{"secrets.sops.yaml", "message authentication code"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"secrets.sops.yaml": tt.file})
			_, err := config.Load([]string{filepath.Join(dir, "secrets.sops.yaml")})
			if err == nil {
				t.Fatal("the document should be rejected")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the error carries a secret: %v", err)
			}
		})
	}
}

func TestOnlyASecretMayBeEncrypted(t *testing.T) {
	site := "apiVersion: clusterctl/v1alpha1\nkind: Site\nmetadata:\n  name: example\nspec:\n  domains:\n    hpc: example.org\n"
	dir := writeFiles(t, map[string]string{"site.yaml": string(sopstest.EncryptWithRegex(t, site, "^spec$", mustRecipient(t)))})
	_, err := config.Load([]string{filepath.Join(dir, "site.yaml")})
	if err == nil || !strings.Contains(err.Error(), "only a Secret document may be") {
		t.Errorf("an encrypted Site should be refused, got %v", err)
	}
}

func TestHiddenFilesAreNotConfiguration(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		".sops.yaml":        "creation_rules:\n  - encrypted_regex: ^(data|binaryData)$\n",
		".site.yaml.swp":    "junk",
		"secrets.sops.yaml": sealed(t, plainSecret),
	})
	files, err := config.ExpandEntries([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "secrets.sops.yaml" {
		t.Errorf("files = %v, want only secrets.sops.yaml", files)
	}
}

func TestSecretRefsAreCheckedWhenResolving(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want []string
	}{
		{
			name: "unknown Secret",
			spec: "  credentials:\n    bmc:\n      username: admin\n      password:\n        secretRef: {name: valut, key: bmc-password}\n",
			want: []string{"site.yaml:", `credential "bmc" refers to Secret "valut"`, "known: [vault]"},
		},
		{
			name: "unknown key",
			spec: "  credentials:\n    bmc:\n      username: admin\n      password:\n        secretRef: {name: vault, key: bmc-pasword}\n",
			want: []string{`key "bmc-pasword"`, "[bmc-password munge-key]"},
		},
		{
			name: "unknown key of a secret file",
			spec: "  services:\n    cinc:\n      secrets:\n        - target: /etc/munge/munge.key\n          secretRef: {name: vault, key: munge}\n",
			want: []string{"secret file /etc/munge/munge.key", `key "munge"`},
		},
		{
			name: "both sources",
			spec: "  services:\n    cinc:\n      secrets:\n        - target: /etc/k\n          source: k.age\n          secretRef: {name: vault, key: munge-key}\n",
			want: []string{"services.cinc.secrets[0] (/etc/k)", "exactly one of source and secretRef"},
		},
		{
			name: "neither source",
			spec: "  services:\n    cinc:\n      secrets:\n        - target: /etc/k\n",
			want: []string{"exactly one of source and secretRef"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveWithSecret(t, tt.spec)
			if err == nil {
				t.Fatal("the reference should be refused")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}

	good := "  credentials:\n    bmc:\n      username: admin\n      password:\n        secretRef: {name: vault, key: bmc-password}\n" +
		"  services:\n    cinc:\n      secrets:\n        - target: /etc/munge/munge.key\n          secretRef: {name: vault, key: munge-key}\n"
	r, err := resolveWithSecret(t, good)
	if err != nil {
		t.Fatalf("valid references were refused: %v", err)
	}
	if ref := r.Spec.Credentials["bmc"].Password.SecretRef; ref == nil || ref.String() != "vault/bmc-password" {
		t.Errorf("secretRef = %v, want vault/bmc-password", ref)
	}
}

func TestSecretValuesDecodesBinaryData(t *testing.T) {
	sections := map[string]map[string]string{
		"data":       {"bmc-password": "hunter2", "pin": "0600"},
		"binaryData": {"munge-key": "bXVu\nZ2U="},
	}
	values, err := config.SecretValues("s.yaml", sections)
	if err != nil {
		t.Fatalf("SecretValues failed: %v", err)
	}
	for key, want := range map[string]string{"bmc-password": "hunter2", "munge-key": "munge", "pin": "0600"} {
		if got := string(values[key]); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	sections["binaryData"]["munge-key"] = "hunter2!"
	_, err = config.SecretValues("s.yaml", sections)
	if err == nil {
		t.Fatal("binaryData that is not base64 should be refused")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error holds the value: %v", err)
	}
}

// resolveWithSecret resolves a minimal site with the given spec next to an
// encrypted Secret named vault.
func resolveWithSecret(t *testing.T, spec string) (*config.Resolved, error) {
	t.Helper()
	dir := writeFiles(t, map[string]string{
		"config.yaml":       "apiVersion: clusterctl/v1alpha1\nkind: Config\ncontexts:\n  - name: c\n    cluster: c\n",
		"cluster.yaml":      "apiVersion: clusterctl/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  site: s\n",
		"site.yaml":         "apiVersion: clusterctl/v1alpha1\nkind: Site\nmetadata:\n  name: s\nspec:\n" + spec,
		"secrets.sops.yaml": sealed(t, plainSecret),
	})
	files, err := config.ExpandEntries([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.Load(files)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	return b.Resolve(config.ResolveOptions{Env: func(string) string { return "" }})
}

func mustRecipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}
