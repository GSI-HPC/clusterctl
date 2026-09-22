// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"

	"github.com/GSI-HPC/clusterctl/internal/secrets"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
)

const secretDoc = `apiVersion: clusterctl/v1alpha1
kind: Secret
metadata:
  name: example
data:
  bmc-password: hunter2
`

func TestSopsRoundTrip(t *testing.T) {
	t.Parallel()

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := age.GenerateX25519Identity()
	file := sopstest.Encrypt(t, secretDoc, id.Recipient().String(), other.Recipient().String())

	// Only the values under data are encrypted; the rest stays readable.
	for _, want := range []string{"kind: Secret", "name: example", "bmc-password: ENC[AES256_GCM"} {
		if !strings.Contains(string(file), want) {
			t.Errorf("the encrypted file is missing %q:\n%s", want, file)
		}
	}
	if strings.Contains(string(file), "hunter2") {
		t.Fatal("the plaintext is in the encrypted file")
	}

	info, err := secrets.InspectSops(file)
	if err != nil {
		t.Fatalf("InspectSops failed: %v", err)
	}
	if got, want := info.Summary(), "2 age"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	if info.Keys[0].ID != id.Recipient().String() {
		t.Errorf("first key = %q, want the recipient", info.Keys[0].ID)
	}

	plain, err := secrets.DecryptSops(file, []age.Identity{other})
	if err != nil {
		t.Fatalf("DecryptSops failed: %v", err)
	}
	if !strings.Contains(string(plain), "bmc-password: hunter2") {
		t.Errorf("plaintext = %s", plain)
	}
}

func TestSopsOpensWithAnSSHIdentity(t *testing.T) {
	t.Parallel()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	id, err := agessh.NewEd25519Identity(priv)
	if err != nil {
		t.Fatal(err)
	}
	recipient := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	file := sopstest.Encrypt(t, secretDoc, recipient)

	if _, err := secrets.DecryptSops(file, []age.Identity{id}); err != nil {
		t.Fatalf("an ssh identity from workstation.identities should open the file: %v", err)
	}
}

func TestSopsRefusesTampering(t *testing.T) {
	t.Parallel()

	id, _ := age.GenerateX25519Identity()
	file := string(sopstest.Encrypt(t, secretDoc, id.Recipient().String()))

	// The name is in the clear but covered by the message authentication
	// code, so renaming the document is caught when it is decrypted.
	renamed := strings.Replace(file, "name: example", "name: other", 1)
	if _, err := secrets.DecryptSops([]byte(renamed), []age.Identity{id}); err == nil {
		t.Error("a document edited without sops should not decrypt")
	}

	stranger, _ := age.GenerateX25519Identity()
	if _, err := secrets.DecryptSops([]byte(file), []age.Identity{stranger}); err == nil {
		t.Error("a foreign identity should not decrypt the file")
	}
}

func TestInspectSopsRejectsWhatSopsDidNotWrite(t *testing.T) {
	t.Parallel()

	for name, file := range map[string]string{
		"plaintext": secretDoc,
		"no keys":   secretDoc + "sops:\n  mac: x\n  version: 3.13.3\n",
		"not yaml":  "\t:",
	} {
		if _, err := secrets.InspectSops([]byte(file)); err == nil {
			t.Errorf("%s: InspectSops should fail", name)
		}
	}
}
