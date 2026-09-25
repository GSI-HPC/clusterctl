// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package sopstest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/secrets"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
)

// Every package has a test file, which is also what keeps "go test -cover
// ./..." working: under go1.26.0 a package without one fails the coverage
// run with 'no such tool "covdata"'.

const doc = `apiVersion: clusterctl/v1alpha1
kind: Secret
metadata:
  name: example
data:
  bmc-password: hunter2
`

// The files these helpers write stand in for what an administrator's sops
// writes in every test that reads a secret, so they have to be files the
// reader decrypts, with exactly the values the rule names encrypted.
func TestEncryptWritesWhatTheReaderDecrypts(t *testing.T) {
	t.Parallel()

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := secrets.IdentityFiles([]string{identity})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		file      []byte
		plain     []string
		encrypted []string
	}{
		{
			name:      "the documented rule encrypts data alone",
			file:      sopstest.Encrypt(t, doc, id.Recipient().String()),
			plain:     []string{"kind: Secret", "name: example"},
			encrypted: []string{"bmc-password: ENC[AES256_GCM"},
		},
		{
			name:      "an empty rule encrypts every value",
			file:      sopstest.EncryptWithRegex(t, doc, "", id.Recipient().String()),
			encrypted: []string{"kind: ENC[AES256_GCM", "name: ENC[AES256_GCM", "bmc-password: ENC[AES256_GCM"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, want := range append(tt.plain, tt.encrypted...) {
				if !strings.Contains(string(tt.file), want) {
					t.Errorf("the file is missing %q:\n%s", want, tt.file)
				}
			}
			if strings.Contains(string(tt.file), "hunter2") {
				t.Fatalf("the plaintext is in the file:\n%s", tt.file)
			}
			s := &secrets.Sops{Binary: sopstest.Binary(t)}
			values, err := secrets.DecryptSops(context.Background(), s, tt.file, secrets.SopsKeys{Identities: ids}, []string{"data"})
			if err != nil {
				t.Fatalf("DecryptSops: %v", err)
			}
			if got := values["data"]["bmc-password"]; got != "hunter2" {
				t.Errorf("data.bmc-password = %q, want hunter2", got)
			}
		})
	}
}
