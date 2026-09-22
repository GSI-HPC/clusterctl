// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"filippo.io/age"
	"golang.org/x/crypto/ssh"

	"github.com/GSI-HPC/clusterctl/internal/secrets"
)

func TestInlineRoundTrip(t *testing.T) {
	t.Parallel()

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recs, err := secrets.ParseRecipients([]string{"# the team", "", id.Recipient().String()})
	if err != nil {
		t.Fatalf("ParseRecipients failed: %v", err)
	}
	armored, err := secrets.EncryptArmored([]byte("hunter2\n"), recs)
	if err != nil {
		t.Fatalf("EncryptArmored failed: %v", err)
	}
	if !secrets.IsArmored(armored) || strings.Contains(armored, "hunter2") {
		t.Fatalf("the output is not an armored ciphertext:\n%s", armored)
	}

	types, err := secrets.Inspect(armored)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}
	if len(types) != 1 || types[0] != "X25519" {
		t.Errorf("stanzas = %v, want [X25519]", types)
	}

	// A block scalar pasted one level too deep still reads.
	got, err := secrets.DecryptArmoredString("  "+armored, []age.Identity{id})
	if err != nil {
		t.Fatalf("DecryptArmoredString failed: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("plaintext = %q, want %q", got, "hunter2")
	}

	other, _ := age.GenerateX25519Identity()
	if _, err := secrets.DecryptArmored(armored, []age.Identity{other}); err == nil {
		t.Error("a foreign identity should not decrypt the secret")
	}
}

func TestInspectRejectsDamage(t *testing.T) {
	t.Parallel()

	id, _ := age.GenerateX25519Identity()
	armored, err := secrets.EncryptArmored([]byte("the munge key"), []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(armored), "\n")

	for name, value := range map[string]string{
		"truncated":     strings.Join(lines[:len(lines)-1], "\n"),
		"not base64":    strings.Replace(armored, lines[1], "!"+lines[1][1:], 1),
		"trailing text": armored + "and a comment\n",
		"no header":     strings.Join(lines[1:], "\n"),
		"not age":       "-----BEGIN AGE ENCRYPTED FILE-----\naGVsbG8=\n-----END AGE ENCRYPTED FILE-----\n",
	} {
		if _, err := secrets.Inspect(value); err == nil {
			t.Errorf("%s: Inspect should fail", name)
		}
	}
}

func TestParseRecipientsAcceptsSSHKeys(t *testing.T) {
	t.Parallel()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " admin@desk01"
	recs, err := secrets.ParseRecipients([]string{line})
	if err != nil {
		t.Fatalf("ParseRecipients failed: %v", err)
	}
	armored, err := secrets.EncryptArmored([]byte("x"), recs)
	if err != nil {
		t.Fatal(err)
	}
	types, err := secrets.Inspect(armored)
	if err != nil || len(types) != 1 || types[0] != "ssh-ed25519" {
		t.Errorf("Inspect = %v, %v; want [ssh-ed25519]", types, err)
	}
}

func TestParseRecipientsRefusesWhatIsNotAPublicKey(t *testing.T) {
	t.Parallel()

	id, _ := age.GenerateX25519Identity()
	for _, entry := range []string{id.String(), "age1yubikey1qwerty", "hello"} {
		_, err := secrets.ParseRecipients([]string{entry})
		if err == nil {
			t.Errorf("ParseRecipients(%.20q) should fail", entry)
			continue
		}
		// A private key pasted as a recipient must not be echoed back.
		if strings.Contains(err.Error(), id.String()) {
			t.Errorf("the error repeats the private key: %v", err)
		}
	}
	if _, err := secrets.EncryptArmored([]byte("x"), nil); err == nil {
		t.Error("encrypting to nobody should fail")
	}
}
