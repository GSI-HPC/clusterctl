// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package sopstest writes sops encrypted files for tests, the way
// "sops --encrypt --encrypted-regex '^(data|binaryData)$'" writes them, so
// that no test depends on a sops binary or on a private key kept in the
// repository.
package sopstest

import (
	"testing"
	"time"

	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keys"
	sopsyaml "github.com/getsops/sops/v3/stores/yaml"
)

// sopsVersion is what the metadata says wrote the file. The version package
// of sops is not imported for it, because it pulls in the sops command line.
const sopsVersion = "3.13.3"

// EncryptedRegex is the rule the documentation tells administrators to use.
const EncryptedRegex = "^(data|binaryData)$"

// Encrypt encrypts a plaintext YAML document to the given age recipients and
// returns the file sops would have written.
func Encrypt(t testing.TB, plaintext string, recipients ...string) []byte {
	t.Helper()
	return EncryptWithRegex(t, plaintext, EncryptedRegex, recipients...)
}

// EncryptWithRegex is Encrypt with another encrypted_regex; an empty one
// encrypts every value, which is what sops does by default.
func EncryptWithRegex(t testing.TB, plaintext, regex string, recipients ...string) []byte {
	t.Helper()

	store := sopsyaml.NewStore(&config.YAMLStoreConfig{})
	branches, err := store.LoadPlainFile([]byte(plaintext))
	if err != nil {
		t.Fatalf("sopstest: reading the plaintext: %v", err)
	}
	group := sops.KeyGroup{}
	for _, r := range recipients {
		mk, err := sopsage.MasterKeyFromRecipient(r)
		if err != nil {
			t.Fatalf("sopstest: %v", err)
		}
		group = append(group, keys.MasterKey(mk))
	}
	tree := sops.Tree{
		Branches: branches,
		Metadata: sops.Metadata{
			KeyGroups:      []sops.KeyGroup{group},
			EncryptedRegex: regex,
			Version:        sopsVersion,
		},
	}
	dataKey, errs := tree.GenerateDataKey()
	if len(errs) > 0 {
		t.Fatalf("sopstest: generating the data key: %v", errs)
	}
	cipher := aes.NewCipher()
	mac, err := tree.Encrypt(dataKey, cipher)
	if err != nil {
		t.Fatalf("sopstest: encrypting: %v", err)
	}
	tree.Metadata.LastModified = time.Now().UTC()
	tree.Metadata.MessageAuthenticationCode, err = cipher.Encrypt(mac, dataKey,
		tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("sopstest: encrypting the message authentication code: %v", err)
	}
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		t.Fatalf("sopstest: writing: %v", err)
	}
	return out
}
