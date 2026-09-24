// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

var sections = []string{"data", "binaryData"}

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

	values, err := secrets.DecryptSops(file, secrets.SopsKeys{Identities: []age.Identity{other}}, sections)
	if err != nil {
		t.Fatalf("DecryptSops failed: %v", err)
	}
	if got := values["data"]["bmc-password"]; got != "hunter2" {
		t.Errorf("data.bmc-password = %q, want hunter2", got)
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

	if _, err := secrets.DecryptSops(file, secrets.SopsKeys{Identities: []age.Identity{id}}, sections); err != nil {
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
	if _, err := secrets.DecryptSops([]byte(renamed), secrets.SopsKeys{Identities: []age.Identity{id}}, sections); err == nil {
		t.Error("a document edited without sops should not decrypt")
	}

	stranger, _ := age.GenerateX25519Identity()
	if _, err := secrets.DecryptSops([]byte(file), secrets.SopsKeys{Identities: []age.Identity{stranger}}, sections); err == nil {
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

// TestSopsKeepsPlaintextOutOfErrors is report section 8.1 where the loader
// cannot help: a file changed after the configuration loaded. sops parses
// the plaintext as the unauthenticated type tag says, and its parser quotes
// the value; bytes made it panic.
func TestSopsKeepsPlaintextOutOfErrors(t *testing.T) {
	t.Parallel()

	id, _ := age.GenerateX25519Identity()
	file := string(sopstest.Encrypt(t, secretDoc, id.Recipient().String()))
	for _, typ := range []string{"int", "float", "bool", "time", "bytes", "comment"} {
		retyped := strings.Replace(file, ",type:str]\n", ",type:"+typ+"]\n", 1)
		_, err := secrets.DecryptSops([]byte(retyped), secrets.SopsKeys{Identities: []age.Identity{id}}, sections)
		if err == nil {
			t.Errorf("type:%s: the file decrypted", typ)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("type:%s: the error holds the plaintext: %v", typ, err)
		}
	}

	// A value whose ciphertext was changed opens nothing either, and says
	// nothing of what it held.
	broken := strings.Replace(file, "bmc-password: ENC[AES256_GCM,data:", "bmc-password: ENC[AES256_GCM,data:AAAA", 1)
	if _, err := secrets.DecryptSops([]byte(broken), secrets.SopsKeys{Identities: []age.Identity{id}}, sections); err == nil ||
		!strings.Contains(err.Error(), "could not be decrypted or was changed without sops") {
		t.Errorf("a changed ciphertext: err = %v", err)
	}
}

// TestSopsValuesComeBackAsWritten is report section 8.2: the values are
// read from the decrypted tree, so what another YAML parser would refuse or
// change comes back as it was encrypted.
func TestSopsValuesComeBackAsWritten(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"newlines":  "\n\n\n",
		"tabbed":    "\tx\ny",
		"separator": "a\u2028b",
		"trailing":  "line\n\n",
		"colon":     "a: b",
	}
	doc := secretDoc
	for key, value := range values {
		doc += "  " + key + ": " + strconv.Quote(value) + "\n"
	}
	id, _ := age.GenerateX25519Identity()
	file := sopstest.Encrypt(t, doc+"# a comment, encrypted too\n", id.Recipient().String())
	got, err := secrets.DecryptSops(file, secrets.SopsKeys{Identities: []age.Identity{id}}, sections)
	if err != nil {
		t.Fatalf("DecryptSops failed: %v", err)
	}
	for key, want := range values {
		if got["data"][key] != want {
			t.Errorf("%s = %q, want %q", key, got["data"][key], want)
		}
	}
	if _, ok := got["metadata"]; ok {
		t.Error("a section that was not asked for was returned")
	}
}

// TestSopsRefusesAMACOverEncryptedValuesOnly is report section 8.7: under
// mac_only_encrypted the name is not authenticated.
func TestSopsRefusesAMACOverEncryptedValuesOnly(t *testing.T) {
	t.Parallel()

	id, _ := age.GenerateX25519Identity()
	file := string(sopstest.EncryptWith(t, secretDoc,
		sopstest.Options{Regex: sopstest.EncryptedRegex, MACOnlyEncrypted: true}, id.Recipient().String()))
	renamed := strings.Replace(file, "name: example", "name: other", 1)
	if _, err := secrets.InspectSops([]byte(renamed)); err == nil || !strings.Contains(err.Error(), "mac_only_encrypted") {
		t.Errorf("InspectSops: err = %v, want mac_only_encrypted refused", err)
	}
	if _, err := secrets.DecryptSops([]byte(renamed), secrets.SopsKeys{Identities: []age.Identity{id}}, sections); err == nil {
		t.Error("a renamed file under mac_only_encrypted decrypted")
	}
}

func TestSopsValueType(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"ENC[AES256_GCM,data:abc=,iv:def=,tag:ghi=,type:str]":     "str",
		"ENC[AES256_GCM,data:abc=,iv:def=,tag:ghi=,type:int]":     "int",
		"ENC[AES256_GCM,data:abc,type:int=,iv:d,tag:g,type:bool]": "bool",
		"ENC[x]":  "",
		"hunter2": "",
	} {
		got, ok := secrets.SopsValueType(in)
		if got != want || ok != (want != "") {
			t.Errorf("SopsValueType(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// vault stands in for a Vault server and counts the requests it receives.
func vault(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "permission denied", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestSopsTrustsOnlyTheConfiguredKeyTypes is report section 8.3: an
// hc_vault key added to the unauthenticated metadata made DecryptSops send
// the Vault token to the address it named.
func TestSopsTrustsOnlyTheConfiguredKeyTypes(t *testing.T) {
	srv, requests := vault(t)
	t.Setenv("VAULT_TOKEN", "hvs.ADMIN-SECRET-TOKEN")
	id, _ := age.GenerateX25519Identity()
	file := sopstest.AddVaultKey(t, sopstest.Encrypt(t, secretDoc, id.Recipient().String()), srv.URL)

	info, err := secrets.InspectSops(file)
	if err != nil {
		t.Fatalf("InspectSops failed: %v", err)
	}
	if err := info.CheckKeyTypes(nil); err == nil || !strings.Contains(err.Error(), "hc_vault") {
		t.Errorf("CheckKeyTypes: err = %v, want hc_vault untrusted", err)
	}
	if err := info.CheckKeyTypes([]string{"age", "hc_vault"}); err != nil {
		t.Errorf("CheckKeyTypes with hc_vault trusted: %v", err)
	}

	_, err = secrets.DecryptSops(file, secrets.SopsKeys{Identities: []age.Identity{id}, Discover: true}, sections)
	if err == nil || !strings.Contains(err.Error(), "workstation.sopsKeyTypes") {
		t.Errorf("DecryptSops: err = %v, want the file refused", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the Vault named by the file received %d requests", n)
	}
}

// TestSopsTriesAgeFirst is report section 8.4: with no decryption order,
// sops tried an hc_vault key before the age key of the same group, whatever
// order the identities were given in.
func TestSopsTriesAgeFirst(t *testing.T) {
	srv, requests := vault(t)
	t.Setenv("VAULT_TOKEN", "hvs.ADMIN-SECRET-TOKEN")
	id, _ := age.GenerateX25519Identity()
	file := sopstest.AddVaultKey(t, sopstest.Encrypt(t, secretDoc, id.Recipient().String()), srv.URL)
	keys := secrets.SopsKeys{Identities: []age.Identity{id}, Discover: true, Types: []string{"hc_vault", "age"}}

	if _, err := secrets.DecryptSops(file, keys, sections); err != nil {
		t.Fatalf("DecryptSops failed: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the Vault was asked %d times before the age identity was tried", n)
	}
}

// TestSopsDiscoversKeysOnlyWhenAsked is report section 10.6: sops' own key
// discovery runs programs and asks for passphrases, so without a terminal
// only the given identities are used.
func TestSopsDiscoversKeysOnlyWhenAsked(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys.txt")
	marker := filepath.Join(dir, "ran")
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte("#!/bin/sh\ntouch "+marker+"\ncat "+keyFile+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_CMD", probe)
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	file := sopstest.Encrypt(t, secretDoc, id.Recipient().String())

	_, err := secrets.DecryptSops(file, secrets.SopsKeys{}, sections)
	if err == nil || !strings.Contains(err.Error(), "workstation.identities") {
		t.Errorf("DecryptSops without discovery: err = %v, want a refusal naming workstation.identities", err)
	}
	stranger, _ := age.GenerateX25519Identity()
	_, err = secrets.DecryptSops(file, secrets.SopsKeys{Identities: []age.Identity{stranger}}, sections)
	if err == nil || strings.Contains(err.Error(), keyFile) || strings.Contains(err.Error(), "via sops") {
		t.Errorf("DecryptSops without discovery: err = %v, want only the identities tried", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("SOPS_AGE_KEY_CMD ran without discovery")
	}

	if _, err := secrets.DecryptSops(file, secrets.SopsKeys{Discover: true}, sections); err != nil {
		t.Errorf("DecryptSops with discovery: %v", err)
	}
}
