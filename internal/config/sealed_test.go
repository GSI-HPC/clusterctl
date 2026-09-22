// SPDX-License-Identifier: LGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/secrets"
)

// armored encrypts a value to a fresh identity and indents the armor for a
// block scalar at the given depth.
func armored(t *testing.T, plaintext string, indent int) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	out, err := secrets.EncryptArmored([]byte(plaintext), []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat(" ", indent)
	return pad + strings.ReplaceAll(strings.TrimSpace(out), "\n", "\n"+pad)
}

func loadSite(t *testing.T, spec string) error {
	t.Helper()
	file := filepath.Join(t.TempDir(), "site.yaml")
	src := "apiVersion: clusterctl/v1alpha1\nkind: Site\nmetadata:\n  name: example\nspec:\n" + spec
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load([]string{file})
	return err
}

func TestInlineSecretsAreAccepted(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	err := loadSite(t, `  secrets:
    recipients:
      - `+id.Recipient().String()+`
  credentials:
    bmc:
      username: admin
      password:
        age: |
`+armored(t, "hunter2", 10)+`
  services:
    cinc:
      secrets:
        - target: /etc/munge/munge.key
          age: |
`+armored(t, "the munge key", 12)+`
`)
	if err != nil {
		t.Fatalf("a valid inline secret was rejected: %v", err)
	}
}

func TestInlineSecretsAreCheckedWhereWritten(t *testing.T) {
	lines := strings.Split(armored(t, "hunter2", 10), "\n")
	truncated := strings.Join(lines[:len(lines)-1], "\n")

	tests := []struct {
		name string
		spec string
		want []string
	}{
		{
			name: "truncated paste",
			spec: "  credentials:\n    bmc:\n      username: admin\n      password:\n        age: |\n" + truncated + "\n",
			want: []string{"site.yaml:10:9", "credentials.bmc.password.age", "truncated"},
		},
		{
			name: "ciphertext under another key",
			spec: "  domains:\n    hpc: |\n" + armored(t, "x", 6) + "\n",
			want: []string{"site.yaml:7:5", "spec.domains.hpc", `field named "age"`},
		},
		{
			name: "not armor",
			spec: "  credentials:\n    bmc:\n      username: admin\n      password:\n        age: hunter2\n",
			want: []string{"credentials.bmc.password.age", "does not match pattern"},
		},
		{
			name: "both sources",
			spec: "  services:\n    cinc:\n      secrets:\n        - target: /etc/k\n          source: k.age\n          age: |\n" +
				armored(t, "k", 12) + "\n",
			want: []string{"spec.services.cinc.secrets[0]", "exactly one of source and age"},
		},
		{
			name: "neither source",
			spec: "  services:\n    cinc:\n      secrets:\n        - target: /etc/k\n",
			want: []string{"spec.services.cinc.secrets[0]", "exactly one of source and age"},
		},
		{
			name: "private key as recipient",
			spec: "  secrets:\n    recipients:\n      - AGE-SECRET-KEY-1QQQQ\n",
			want: []string{"site.yaml:7:5", "spec.secrets.recipients[0]", "not an age or ssh public key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadSite(t, tt.spec)
			if err == nil {
				t.Fatal("the document should be rejected")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "AGE-SECRET-KEY") {
				t.Errorf("the error repeats a private key: %v", err)
			}
		})
	}
}

func TestFormatValueSummarizesInlineSecrets(t *testing.T) {
	value := armored(t, "hunter2", 0) + "\n"
	for _, v := range []any{
		value,
		map[string]any{"age": value, "target": "/etc/k"},
		[]any{map[string]any{"age": value}},
	} {
		got := config.FormatValue(v)
		if strings.Contains(got, "BEGIN AGE") || !strings.Contains(got, "<age encrypted,") {
			t.Errorf("FormatValue(%T) = %q, want the ciphertext summarized", v, got)
		}
	}
}
