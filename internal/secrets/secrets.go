// SPDX-License-Identifier: LGPL-3.0-or-later

// Package secrets decrypts the age encrypted files a site keeps beside its
// configuration.
//
// Plaintext is returned in memory and streamed to wherever it is needed. It
// is never written to the workstation's disk, which is what the exec wrappers
// of the shell toolkit were careful about and what this package keeps.
package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
)

// Identities loads the age identities used to decrypt.
//
// A file may hold age identities or an OpenSSH private key; both are
// accepted, because a site usually already has ssh keys and no reason to
// issue a second kind.
func Identities(paths []string) ([]age.Identity, error) {
	var out []age.Identity
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading the identity %s: %w", path, err)
		}
		ids, err := parseIdentities(path, data)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no age identity is configured; set workstation.identities")
	}
	return out, nil
}

func parseIdentities(path string, data []byte) ([]age.Identity, error) {
	if bytes.Contains(data, []byte("PRIVATE KEY")) {
		id, err := agessh.ParseIdentity(data)
		if err != nil {
			return nil, fmt.Errorf("reading the ssh identity %s: %w", path, err)
		}
		return []age.Identity{id}, nil
	}
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reading the age identity %s: %w", path, err)
	}
	return ids, nil
}

// Decrypt reads an age encrypted file and returns its plaintext.
func Decrypt(path string, identities []age.Identity) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the secret %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	r, err := age.Decrypt(f, identities...)
	if err != nil {
		return nil, fmt.Errorf("decrypting %s: %w", path, err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		return nil, fmt.Errorf("decrypting %s: %w", path, err)
	}
	return out.Bytes(), nil
}

// DecryptString decrypts a file whose content is one line, such as a
// password, and returns it without the trailing newline.
func DecryptString(path string, identities []age.Identity) (string, error) {
	data, err := Decrypt(path, identities)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
