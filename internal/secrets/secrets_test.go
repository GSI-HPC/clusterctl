// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/secrets"
)

// encrypted writes an age encrypted file and returns its path and the
// identity file that opens it.
func encrypted(t *testing.T, plaintext string) (secretPath, identityPath string) {
	t.Helper()

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	identityPath = filepath.Join(dir, "identity")
	if err := os.WriteFile(identityPath, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, plaintext); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	secretPath = filepath.Join(dir, "secret.age")
	if err := os.WriteFile(secretPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return secretPath, identityPath
}

func TestDecrypt(t *testing.T) {
	t.Parallel()

	secretPath, identityPath := encrypted(t, "the munge key\n")
	ids, err := secrets.Identities([]string{identityPath})
	if err != nil {
		t.Fatalf("Identities failed: %v", err)
	}

	data, err := secrets.Decrypt(secretPath, ids)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if got, want := string(data), "the munge key\n"; got != want {
		t.Errorf("Decrypt = %q, want %q", got, want)
	}
}

func TestDecryptWithTheWrongIdentity(t *testing.T) {
	t.Parallel()

	secretPath, _ := encrypted(t, "secret")
	_, otherIdentity := encrypted(t, "other")

	ids, err := secrets.Identities([]string{otherIdentity})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Decrypt(secretPath, ids); err == nil {
		t.Error("decrypting with the wrong identity should fail")
	}
}

func TestIdentitiesReportsProblems(t *testing.T) {
	t.Parallel()

	if _, err := secrets.Identities(nil); err == nil {
		t.Error("no identity at all should be reported")
	} else if !strings.Contains(err.Error(), "workstation.identities") {
		t.Errorf("error = %v, want it to say where to configure one", err)
	}

	if _, err := secrets.Identities([]string{filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("a missing identity file should be reported")
	}

	junk := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junk, []byte("not an identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Identities([]string{junk}); err == nil {
		t.Error("a file that is not an identity should be reported")
	}
}

func TestSSHIdentityIsAccepted(t *testing.T) {
	t.Parallel()

	// A site usually has ssh keys already and no reason to issue a second
	// kind, so an OpenSSH private key works as an age identity.
	key := `-----BEGIN OPENSSH PRIVATE KEY-----
not a real key
-----END OPENSSH PRIVATE KEY-----
`
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := secrets.Identities([]string{path})
	if err == nil {
		t.Fatal("a malformed ssh key should be reported")
	}
	if !strings.Contains(err.Error(), "ssh identity") {
		t.Errorf("error = %v, want it to say the file was read as an ssh identity", err)
	}
}
