// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"filippo.io/age"
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

// realSops is the sops in PATH, which the test fails without.
func realSops(t *testing.T) *secrets.Sops {
	t.Helper()
	return &secrets.Sops{Binary: sopstest.Binary(t)}
}

// identities writes each identity into a file of its own and reads them the
// way workstation.identities is read.
func identities(t *testing.T, contents ...string) []secrets.IdentityFile {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i, c := range contents {
		p := filepath.Join(dir, "identity"+strconv.Itoa(i))
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	files, err := secrets.IdentityFiles(paths)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// sshKey returns a new OpenSSH private key and its public key as an age
// recipient.
func sshKey(t *testing.T) (private, recipient string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " admin@example"
}

// fakeSops writes a sops that says it is version and then runs script,
// and records in the returned file that it was run.
func fakeSops(t *testing.T, version, script string) (*secrets.Sops, string) {
	t.Helper()
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	path := filepath.Join(dir, "sops")
	body := "#!/bin/sh\n" +
		"case \"$*\" in *--version*) echo 'sops " + version + "'; exit 0;; esac\n" +
		"printf '%s\\n' \"$*\" > " + ran + "\n" +
		"env >> " + ran + "\n" +
		script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return &secrets.Sops{Binary: path}, ran
}

func decrypt(t *testing.T, s *secrets.Sops, file []byte, keys secrets.SopsKeys) (map[string]map[string]string, error) {
	t.Helper()
	return secrets.DecryptSops(context.Background(), s, file, keys, sections)
}

func TestSopsRoundTrip(t *testing.T) {
	t.Parallel()

	id, other := newIdentity(t), newIdentity(t)
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
	if info.LastModified.IsZero() || info.EncryptedRegex != sopstest.EncryptedRegex {
		t.Errorf("InspectSops = %+v, want the time and the rule sops wrote", info)
	}

	// The identity of the second recipient opens the file too: sops reads
	// the identity file again for every key it tries.
	values, err := decrypt(t, realSops(t), file, secrets.SopsKeys{Identities: identities(t, other.String()+"\n")})
	if err != nil {
		t.Fatalf("DecryptSops failed: %v", err)
	}
	if got := values["data"]["bmc-password"]; got != "hunter2" {
		t.Errorf("data.bmc-password = %q, want hunter2", got)
	}
}

func TestSopsOpensWithAnSSHIdentity(t *testing.T) {
	t.Parallel()

	key, recipient := sshKey(t)
	file := sopstest.Encrypt(t, secretDoc, recipient)
	if _, err := decrypt(t, realSops(t), file, secrets.SopsKeys{Identities: identities(t, key)}); err != nil {
		t.Fatalf("an ssh identity from workstation.identities should open the file: %v", err)
	}
}

// sops takes one OpenSSH key and one age key file per run, so each file of
// workstation.identities that holds a recipient of the file gets a run of
// its own, and the others none.
func TestSopsTriesEveryIdentityThatIsARecipient(t *testing.T) {
	t.Parallel()

	key1, recipient1 := sshKey(t)
	key2, recipient2 := sshKey(t)
	id := newIdentity(t)
	stranger := newIdentity(t)
	file := sopstest.Encrypt(t, secretDoc, recipient1, id.Recipient().String())

	// The ssh recipient is relabelled as the second key, which then claims
	// a data key it cannot open: its run fails, the first key is no longer
	// named and gets none, and the age identity after them opens the file.
	info, err := secrets.InspectSops(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Keys[0].ID != recipient1 && info.Keys[1].ID != recipient1 {
		t.Fatalf("keys = %v, want %q among them", info.Keys, recipient1)
	}
	file = []byte(strings.Replace(string(file), recipient1, recipient2, 1))

	keys := secrets.SopsKeys{Identities: identities(t, stranger.String()+"\n", key2, key1, id.String()+"\n")}
	values, err := decrypt(t, realSops(t), file, keys)
	if err != nil {
		t.Fatalf("DecryptSops failed: %v", err)
	}
	if values["data"]["bmc-password"] != "hunter2" {
		t.Errorf("values = %v", values)
	}
}

func TestSopsRefusesTampering(t *testing.T) {
	t.Parallel()

	id := newIdentity(t)
	file := string(sopstest.Encrypt(t, secretDoc, id.Recipient().String()))
	keys := secrets.SopsKeys{Identities: identities(t, id.String()+"\n")}

	// The name is in the clear but covered by the message authentication
	// code, so renaming the document is caught by sops when it decrypts.
	for name, edit := range map[string][2]string{
		"renamed":      {"name: example", "name: other"},
		"retitled key": {"bmc-password:", "ipmi-password:"},
		"no mac":       {"    mac: ENC[", "    mac: xENC["},
	} {
		changed := strings.Replace(file, edit[0], edit[1], 1)
		if changed == file {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		_, err := decrypt(t, realSops(t), []byte(changed), keys)
		if err == nil || !strings.Contains(err.Error(), "changed without sops") {
			t.Errorf("%s: err = %v, want the file refused as changed without sops", name, err)
		}
	}

	stranger := newIdentity(t)
	if _, err := decrypt(t, realSops(t), []byte(file), secrets.SopsKeys{Identities: identities(t, stranger.String()+"\n")}); err == nil {
		t.Error("a foreign identity should not decrypt the file")
	}
}

func TestInspectSopsRejectsWhatSopsDidNotWrite(t *testing.T) {
	t.Parallel()

	id := newIdentity(t)
	file := string(sopstest.Encrypt(t, secretDoc, id.Recipient().String()))
	if _, err := secrets.InspectSops([]byte(file)); err != nil {
		t.Fatalf("InspectSops refused what sops wrote: %v", err)
	}
	group := "    key_groups:\n        - age:\n            - recipient: " + id.Recipient().String() + "\n              enc: x\n"
	for name, tt := range map[string]struct{ file, want string }{
		"plaintext":     {secretDoc, "not encrypted"},
		"no keys":       {secretDoc + "sops:\n  mac: x\n  version: 3.13.3\n", "no key"},
		"not yaml":      {"\t:", "sops metadata"},
		"no mac":        {strings.Replace(file, "    mac: ", "    unused_mac: ", 1), "message authentication code"},
		"unknown field": {strings.Replace(file, "    version: ", "    signed_by: x\n    version: ", 1), "signed_by"},
		"empty mac":     {regexpReplace(file, `(?m)^    mac: .*$`, `    mac: ""`), "message authentication code"},
		"two documents": {file + "---\n" + secretDoc, "2 documents"},
		"unknown kind of key": {strings.Replace(file, "\nsops:\n", "\nsops:\n    future_kms:\n        - id: x\n", 1),
			"future_kms"},
		"unknown field in a group": {secretDoc + "sops:\n" + strings.Replace(group, "- age:", "- future_kms: []\n          age:", 1) +
			"    lastmodified: \"2026-09-25T12:00:00Z\"\n    mac: ENC[x]\n", "future_kms"},
		"keys in and outside key_groups": {strings.Replace(file, "\nsops:\n", "\nsops:\n"+group, 1), "key_groups"},
		"bad lastmodified":               {regexpReplace(file, `(?m)^    lastmodified: .*$`, `    lastmodified: yesterday`), "lastmodified"},
	} {
		_, err := secrets.InspectSops([]byte(tt.file))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: InspectSops: err = %v, want it to mention %q", name, err, tt.want)
		}
	}
}

// TestInspectSopsReadsEveryKindOfKey names the keys the way sops does, in
// the order it reads them, and counts the groups.
func TestInspectSopsReadsEveryKindOfKey(t *testing.T) {
	t.Parallel()

	const meta = `sops:
    key_groups:
        - kms:
            - arn: arn:aws:kms:eu-central-1:1:key/a
              role: arn:aws:iam::1:role/r
              enc: x
          gcp_kms:
            - resource_id: projects/p/locations/l/keyRings/r/cryptoKeys/k
              enc: x
          hckms:
            - key_id: region:uuid
              enc: x
        - azure_kv:
            - vault_url: https://v.vault.azure.net
              name: k
              version: "1"
              enc: x
          hc_vault:
            - vault_address: https://vault:8200
              engine_path: transit
              key_name: k
              enc: x
          pgp:
            - fp: ABCDEF
              enc: x
          age:
            - recipient: age1abc
              enc: x
    shamir_threshold: 2
    lastmodified: "2026-09-25T12:00:00Z"
    mac: ENC[x]
    version: 3.13.3
`
	info, err := secrets.InspectSops([]byte(secretDoc + meta))
	if err != nil {
		t.Fatalf("InspectSops failed: %v", err)
	}
	var got []string
	for _, k := range info.Keys {
		got = append(got, k.Type+" "+k.ID)
	}
	want := []string{
		"kms arn:aws:kms:eu-central-1:1:key/a+arn:aws:iam::1:role/r",
		"gcp_kms projects/p/locations/l/keyRings/r/cryptoKeys/k",
		"hckms region:uuid",
		"azure_kv https://v.vault.azure.net/keys/k/1",
		"hc_vault https://vault:8200/v1/transit/keys/k",
		"pgp ABCDEF",
		"age age1abc",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("keys:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if info.Groups != 2 || info.Threshold != 2 {
		t.Errorf("groups %d, threshold %d; want 2 and 2", info.Groups, info.Threshold)
	}
	if got, want := info.Summary(), "1 age, 1 azure_kv, 1 gcp_kms, 1 hc_vault, 1 hckms, 1 kms, 1 pgp in 2 groups, 2 needed"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}

// TestSopsKeepsPlaintextOutOfErrors is report section 8.1 where the loader
// cannot help: a file changed after the configuration loaded. sops parses
// the plaintext as the unauthenticated type tag says, and its parser quotes
// the value, so such a file is refused before sops runs.
func TestSopsKeepsPlaintextOutOfErrors(t *testing.T) {
	t.Parallel()

	id := newIdentity(t)
	file := string(sopstest.Encrypt(t, secretDoc, id.Recipient().String()))
	keys := secrets.SopsKeys{Identities: identities(t, id.String()+"\n")}
	never, ran := fakeSops(t, "3.13.3", "exit 0")
	for _, typ := range []string{"int", "float", "bool", "time", "bytes", "comment"} {
		retyped := strings.Replace(file, ",type:str]\n", ",type:"+typ+"]\n", 1)
		_, err := decrypt(t, never, []byte(retyped), keys)
		if err == nil || !strings.Contains(err.Error(), "type:"+typ) {
			t.Errorf("type:%s: err = %v, want the type refused", typ, err)
		}
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("sops ran on a file whose type tag was changed")
	}

	// A value whose ciphertext was changed opens nothing either, and says
	// nothing of what it held.
	broken := strings.Replace(file, "bmc-password: ENC[AES256_GCM,data:", "bmc-password: ENC[AES256_GCM,data:AAAA", 1)
	_, err := decrypt(t, realSops(t), []byte(broken), keys)
	if err == nil || !strings.Contains(err.Error(), "without sops") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a changed ciphertext: err = %v", err)
	}
}

// TestSopsSaysNothingOnceTheDataKeyIsOpen: sops' own messages quote a
// decrypted value when it cannot parse it. Only its account of the keys it
// tried is passed on, and that is escaped; what it says on any other
// failure is not.
func TestSopsSaysNothingOnceTheDataKeyIsOpen(t *testing.T) {
	t.Parallel()

	id := newIdentity(t)
	file := sopstest.Encrypt(t, secretDoc, id.Recipient().String())
	keys := secrets.SopsKeys{Identities: identities(t, id.String()+"\n")}

	for code, want := range map[int]string{
		1:  "could not be decrypted",
		25: "could not be decrypted",
		51: "changed without sops",
		52: "changed without sops",
	} {
		s, _ := fakeSops(t, "3.13.3", "echo 'Could not decrypt value: strconv.Atoi: parsing \"hunter2\"' >&2; exit "+strconv.Itoa(code))
		_, err := decrypt(t, s, file, keys)
		if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), want) {
			t.Errorf("exit %d: err = %v, want %q and no plaintext", code, err, want)
		}
	}

	s, _ := fakeSops(t, "3.13.3", "printf 'Group 0: FAILED\\n  age1x: \\033]0;owned\\007FAILED\\n' >&2; exit 128")
	_, err := decrypt(t, s, file, keys)
	if err == nil || !strings.Contains(err.Error(), "age1x:") || strings.ContainsAny(err.Error(), "\x1b\x07\n") {
		t.Errorf("exit 128: err = %q, want what sops tried, escaped, on one line", err)
	}
}

// TestSopsValuesComeBackAsWritten is report section 8.2: sops writes the
// plaintext as JSON, so a value comes back as it was encrypted, with no
// parser to change or refuse it.
func TestSopsValuesComeBackAsWritten(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"newlines":  "\n\n\n",
		"tabbed":    "\tx\ny",
		"separator": "a\u2028b",
		"trailing":  "line\n\n",
		"colon":     "a: b",
		"quote":     `"'\`,
		"leading":   "  x",
		"unicode":   "grüße 🔑",
		"empty":     "",
	}
	doc := secretDoc
	for key, value := range values {
		doc += "  " + key + ": " + strconv.Quote(value) + "\n"
	}
	doc += "binaryData:\n  munge-key: czNjcjN0LWtleQ==\n"
	id := newIdentity(t)
	file := sopstest.Encrypt(t, doc+"# a comment, encrypted too\n", id.Recipient().String())
	got, err := decrypt(t, realSops(t), file, secrets.SopsKeys{Identities: identities(t, id.String()+"\n")})
	if err != nil {
		t.Fatalf("DecryptSops failed: %v", err)
	}
	for key, want := range values {
		if got["data"][key] != want {
			t.Errorf("%s = %q, want %q", key, got["data"][key], want)
		}
	}
	if got["binaryData"]["munge-key"] != "czNjcjN0LWtleQ==" {
		t.Errorf("binaryData = %v", got["binaryData"])
	}
	if _, ok := got["metadata"]; ok {
		t.Error("a section that was not asked for was returned")
	}
}

// TestSopsRefusesAMACOverEncryptedValuesOnly is report section 8.7: under
// mac_only_encrypted the name is not authenticated.
func TestSopsRefusesAMACOverEncryptedValuesOnly(t *testing.T) {
	t.Parallel()

	id := newIdentity(t)
	file := string(sopstest.EncryptWith(t, secretDoc,
		sopstest.Options{Regex: sopstest.EncryptedRegex, MACOnlyEncrypted: true}, id.Recipient().String()))
	renamed := strings.Replace(file, "name: example", "name: other", 1)
	if _, err := secrets.InspectSops([]byte(renamed)); err == nil || !strings.Contains(err.Error(), "mac_only_encrypted") {
		t.Errorf("InspectSops: err = %v, want mac_only_encrypted refused", err)
	}
	never, ran := fakeSops(t, "3.13.3", "exit 0")
	if _, err := decrypt(t, never, []byte(renamed), secrets.SopsKeys{Identities: identities(t, id.String()+"\n")}); err == nil {
		t.Error("a renamed file under mac_only_encrypted decrypted")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("sops ran on a file encrypted with mac_only_encrypted")
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
// the Vault token to the address it named. sops tries every key the
// metadata names, so the file is refused before sops runs at all.
func TestSopsTrustsOnlyTheConfiguredKeyTypes(t *testing.T) {
	srv, requests := vault(t)
	t.Setenv("VAULT_TOKEN", "hvs.ADMIN-SECRET-TOKEN")
	id := newIdentity(t)
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

	never, ran := fakeSops(t, "3.13.3", "exit 0")
	_, err = decrypt(t, never, file, secrets.SopsKeys{Identities: identities(t, id.String()+"\n"), Discover: true})
	if err == nil || !strings.Contains(err.Error(), "workstation.sopsKeyTypes") {
		t.Errorf("DecryptSops: err = %v, want the file refused", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("sops ran on a file encrypted to a key type that is not trusted")
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the Vault named by the file received %d requests", n)
	}
}

// TestSopsTriesAgeFirst is report section 8.4: with no decryption order,
// sops tried an hc_vault key before the age key of the same group. Here sops
// looks for the key itself, with VAULT_TOKEN in its environment, and still
// opens the age key before it contacts the Vault.
func TestSopsTriesAgeFirst(t *testing.T) {
	srv, requests := vault(t)
	id := newIdentity(t)
	keyFile := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VAULT_TOKEN", "hvs.ADMIN-SECRET-TOKEN")
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	t.Setenv("SOPS_DECRYPTION_ORDER", "hc_vault,age")
	file := sopstest.AddVaultKey(t, sopstest.Encrypt(t, secretDoc, id.Recipient().String()), srv.URL)
	keys := secrets.SopsKeys{Discover: true, Types: []string{"hc_vault", "age"}}

	if _, err := decrypt(t, realSops(t), file, keys); err != nil {
		t.Fatalf("DecryptSops failed: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the Vault was asked %d times before the age identity was tried", n)
	}
}

// TestSopsDiscoversKeysOnlyWhenAsked is report section 10.6: sops' own key
// discovery runs programs and asks for passphrases, so without a terminal
// only the given identities are used, and sops finds no key of its own
// even where it would look: the environment, the home directory, the
// configuration directory.
func TestSopsDiscoversKeysOnlyWhenAsked(t *testing.T) {
	id := newIdentity(t)
	sshPriv, sshRecipient := sshKey(t)
	home := t.TempDir()
	keyFile := filepath.Join(home, ".config", "sops", "age", "keys.txt")
	marker := filepath.Join(t.TempDir(), "ran")
	probe := filepath.Join(t.TempDir(), "probe")
	for path, content := range map[string]string{
		keyFile: id.String() + "\n",
		filepath.Join(home, ".ssh", "id_ed25519"): sshPriv,
		probe: "#!/bin/sh\ntouch " + marker + "\ncat " + keyFile + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SOPS_AGE_KEY_CMD", probe)
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	t.Setenv("SOPS_AGE_KEY", id.String())
	t.Setenv("SOPS_AGE_SSH_PRIVATE_KEY_CMD", probe)
	file := sopstest.Encrypt(t, secretDoc, id.Recipient().String(), sshRecipient)

	_, err := decrypt(t, realSops(t), file, secrets.SopsKeys{})
	if err == nil || !strings.Contains(err.Error(), "workstation.identities") {
		t.Errorf("DecryptSops without discovery: err = %v, want a refusal naming workstation.identities", err)
	}

	// An identity that claims to be a recipient but holds another key
	// makes sops run, and it still finds none of the keys around it: the
	// recipient is only a label in the unauthenticated metadata.
	stranger := newIdentity(t)
	claimed := []byte(strings.Replace(string(file), id.Recipient().String(), stranger.Recipient().String(), 1))
	claimed = []byte(strings.Replace(string(claimed), sshRecipient, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGnoRmT3y8H4gCz5X4cJ0fJlmBa3gt7nsgYt9V5Y5a0u", 1))
	_, err = decrypt(t, realSops(t), claimed, secrets.SopsKeys{Identities: identities(t, stranger.String()+"\n")})
	if err == nil || !strings.Contains(err.Error(), "sops says") || strings.Contains(err.Error(), "via sops") {
		t.Errorf("DecryptSops without discovery: err = %v, want only the identity tried", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("SOPS_AGE_KEY_CMD ran without discovery")
	}

	if _, err := decrypt(t, realSops(t), file, secrets.SopsKeys{Discover: true}); err != nil {
		t.Errorf("DecryptSops with discovery: %v", err)
	}
}

// TestSopsIsRunTheOneWay records how sops is run: from stdin to stdout,
// never with --output, --in-place or --ignore-mac, with no configuration
// but an empty one, and without the credentials or key settings of
// clusterctl's environment.
func TestSopsIsRunTheOneWay(t *testing.T) {
	id := newIdentity(t)
	file := sopstest.Encrypt(t, secretDoc, id.Recipient().String())
	t.Setenv("VAULT_TOKEN", "hvs.ADMIN-SECRET-TOKEN")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("SOPS_KEYSERVICE", "tcp://keys.example:5000")
	t.Setenv("SOPS_AGE_KEY", "AGE-SECRET-KEY-1ENVIRONMENT")
	s, ran := fakeSops(t, "3.13.3", `cat >/dev/null; echo '{"data":{"bmc-password":"hunter2"}}'`)
	idFiles := identities(t, id.String()+"\n")

	values, err := decrypt(t, s, file, secrets.SopsKeys{Identities: idFiles})
	if err != nil || values["data"]["bmc-password"] != "hunter2" {
		t.Fatalf("DecryptSops = %v, %v", values, err)
	}
	record, err := os.ReadFile(ran)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(record), "\n")
	args := lines[0]
	if want := "--config " + os.DevNull + " decrypt --input-type yaml --output-type json --decryption-order age,pgp"; args != want {
		t.Errorf("sops was run with %q, want %q", args, want)
	}
	env := strings.Join(lines[1:], "\n")
	for _, bad := range []string{"VAULT_TOKEN", "AWS_", "SOPS_KEYSERVICE", "SOPS_AGE_KEY=", "AGE-SECRET-KEY"} {
		if strings.Contains(env, bad) {
			t.Errorf("sops got %s in its environment:\n%s", bad, env)
		}
	}
	if !strings.Contains(env, "SOPS_AGE_KEY_FILE="+idFiles[0].Path) {
		t.Errorf("sops was not pointed at the identity file:\n%s", env)
	}

	// At a terminal sops looks for keys the way the sops command does,
	// but still without a remote key service.
	s, ran = fakeSops(t, "3.13.3", `cat >/dev/null; echo '{"data":{}}'`)
	if _, err := decrypt(t, s, file, secrets.SopsKeys{Discover: true}); err != nil {
		t.Fatal(err)
	}
	record, _ = os.ReadFile(ran)
	if !strings.Contains(string(record), "VAULT_TOKEN=") || strings.Contains(string(record), "SOPS_KEYSERVICE") {
		t.Errorf("the environment of sops at a terminal:\n%s", record)
	}
}

func TestSopsVersion(t *testing.T) {
	t.Parallel()

	found, err := realSops(t).Find(context.Background())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Path == "" || found.Version == "" {
		t.Errorf("Find = %+v", found)
	}

	id := newIdentity(t)
	file := sopstest.Encrypt(t, secretDoc, id.Recipient().String())
	keys := secrets.SopsKeys{Identities: identities(t, id.String()+"\n")}
	for name, s := range map[string]*secrets.Sops{
		"too old": func() *secrets.Sops { s, _ := fakeSops(t, "3.9.4", "exit 0"); return s }(),
		"missing": {Binary: filepath.Join(t.TempDir(), "sops")},
	} {
		_, err := decrypt(t, s, file, keys)
		if err == nil || !strings.Contains(err.Error(), secrets.MinSopsVersion) {
			t.Errorf("%s: err = %v, want the version needed", name, err)
		}
	}
}

func TestSopsIsNotNeededToInspect(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	const file = secretDoc + `sops:
    age:
        - recipient: age1abc
          enc: x
    lastmodified: "2026-09-25T12:00:00Z"
    mac: ENC[x]
    version: 3.13.3
`
	if _, err := secrets.InspectSops([]byte(file)); err != nil {
		t.Errorf("InspectSops without sops: %v", err)
	}
}

func regexpReplace(s, expr, repl string) string {
	return regexp.MustCompile(expr).ReplaceAllString(s, repl)
}
