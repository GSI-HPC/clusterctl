// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
)

// secretSite writes a Secret named example with the given values, encrypted
// to a fresh key, into a directory the harness reads after the example
// configuration. workstation is added to the spec of the Workstation
// document; with identities the key is named there, and without it only
// sops' own key discovery could find it. edit changes the encrypted file the
// way someone without a key could.
type secretSite struct {
	values      string
	opts        sopstest.Options
	identities  bool
	workstation string
	edit        func([]byte) []byte
	// secrets replaces services.cinc.secrets, so that only files the
	// Secret holds are pushed.
	secrets string
}

func (s secretSite) write(t *testing.T) (dir, keyFile string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	keyFile = filepath.Join(dir, ".identity")
	opts := s.opts
	if opts.Regex == "" {
		opts.Regex = sopstest.EncryptedRegex
	}
	secret := sopstest.EncryptWith(t, "apiVersion: clusterctl/v1alpha1\nkind: Secret\nmetadata:\n  name: example\n"+s.values,
		opts, id.Recipient().String())
	if s.edit != nil {
		secret = s.edit(secret)
	}
	ws := "apiVersion: clusterctl/v1alpha1\nkind: Workstation\nspec:\n  host: test\n"
	if s.identities {
		ws += "  identities:\n    - " + keyFile + "\n"
	}
	ws += s.workstation
	files := map[string]string{
		".identity":         id.String() + "\n",
		"secrets.sops.yaml": string(secret),
		"workstation.yaml":  ws,
	}
	if s.secrets != "" {
		files["override.yaml"] = "apiVersion: clusterctl/v1alpha1\nkind: Config\ncontexts:\n  - name: cluster1\n    cluster: cluster1\n" +
			"    overrides:\n      services.cinc.secrets:\n" + s.secrets
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, keyFile
}

const bmcSecret = "data:\n  bmc-password: hunter2\nbinaryData:\n  munge-key: czNjcjN0LWtleQ==\n"

// retype changes the type sops recorded for the encrypted bmc-password. The
// type is outside what AES-GCM authenticates, so no key is needed.
func retype(to string) func([]byte) []byte {
	return func(file []byte) []byte {
		lines := strings.Split(string(file), "\n")
		for i, line := range lines {
			if strings.Contains(line, "bmc-password: ENC[") {
				lines[i] = strings.Replace(line, ",type:str]", ",type:"+to+"]", 1)
			}
		}
		return []byte(strings.Join(lines, "\n"))
	}
}

// TestSecretTypeTagsAreRefusedBeforeDecrypting is report section 8.1: sops
// parses the plaintext as the type tag says, and the parser's error quotes
// the value. The tag is readable without a key, so the file is refused when
// the configuration loads, and nothing of the value reaches any output.
func TestSecretTypeTagsAreRefusedBeforeDecrypting(t *testing.T) {
	for _, typ := range []string{"int", "float", "bool", "time", "bytes"} {
		t.Run(typ, func(t *testing.T) {
			dir, _ := secretSite{values: bmcSecret, identities: true, edit: retype(typ)}.write(t)

			h, err := run(t, harnessOptions{config: []string{dir}}, "config", "validate")
			if got := exitcode.From(err); got != exitcode.Usage {
				t.Errorf("config validate: exit %d (%v), want %d:\n%s", got, err, exitcode.Usage, h.out)
			}
			for _, want := range []string{"data.bmc-password", "type:" + typ} {
				if err != nil && !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not mention %q: %v", want, err)
				}
			}
			for _, args := range [][]string{
				{"config", "validate"},
				{"secrets", "check", "--decrypt", "-o", "json"},
			} {
				h, err := run(t, harnessOptions{config: []string{dir}}, args...)
				if leaked(h, err) {
					t.Errorf("%s printed the plaintext:\n%v\n%s%s", args, err, h.out, h.errOut)
				}
			}
		})
	}
}

// TestSecretValuesThatSopsWouldRetypeAreRefused is report section 8.6: sops
// stores an unquoted 0600 as the integer 384, which clusterctl then wrote
// onto the node as written. It is refused with how to keep it as written.
func TestSecretValuesThatSopsWouldRetypeAreRefused(t *testing.T) {
	for _, value := range []string{"0600", "007", "True", "2001-12-14", "1.5"} {
		t.Run(value, func(t *testing.T) {
			dir, _ := secretSite{values: withPin(value), identities: true}.write(t)
			_, err := run(t, harnessOptions{config: []string{dir}}, "config", "validate")
			if got := exitcode.From(err); got != exitcode.Usage {
				t.Fatalf("config validate: exit %d (%v), want %d", got, err, exitcode.Usage)
			}
			for _, want := range []string{"data.pin", "quote"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not mention %q: %v", want, err)
				}
			}
		})
	}

	// Quoted, the value is a string and is used as written.
	dir, _ := secretSite{values: withPin(`"0600"`), identities: true}.write(t)
	if h, err := run(t, harnessOptions{config: []string{dir}}, "secrets", "check", "--decrypt"); err != nil {
		t.Fatalf("a quoted value should load and decrypt: %v\n%s", err, h.out)
	}
}

// withPin is bmcSecret with one more value, written as given.
func withPin(value string) string {
	return strings.Replace(bmcSecret, "binaryData:", "  pin: "+value+"\nbinaryData:", 1)
}

// TestSecretValuesAreNotParsedTwice is report section 8.2: the decrypted
// file was written out as YAML and parsed again by another library, whose
// error quoted the lines around a value it could not read back.
func TestSecretValuesAreNotParsedTwice(t *testing.T) {
	dir, _ := secretSite{
		values:     "data:\n  bmc-password: hunter2\n  spacer: \"\\n\"\n  tabbed: \"\\tx\\ny\"\n  separator: \"a\\u2028b\"\nbinaryData:\n  munge-key: czNjcjN0LWtleQ==\n",
		identities: true,
	}.write(t)
	h, err := run(t, harnessOptions{config: []string{dir}}, "secrets", "check", "--decrypt", "-o", "json")
	if err != nil {
		t.Fatalf("secrets check --decrypt failed: %v\n%s", err, h.out)
	}
	if !strings.Contains(h.out.String(), `"status": "decrypts"`) || leaked(h, err) {
		t.Errorf("the check should decrypt and print nothing of the secret:\n%s", h.out)
	}
}

// vaultServer counts the requests a made-up Vault receives, and whether they
// carried the token.
func vaultServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Logf("the Vault received %s %s with token %q", r.Method, r.URL.Path, r.Header.Get("X-Vault-Token"))
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestSecretsTrustOnlyTheConfiguredKeyTypes is report sections 8.3 and 8.4:
// an hc_vault key added to the metadata, which the message authentication
// code does not cover, made clusterctl send the administrator's Vault token
// to the address it named, before the age identity was even tried.
func TestSecretsTrustOnlyTheConfiguredKeyTypes(t *testing.T) {
	srv, requests := vaultServer(t)
	t.Setenv("VAULT_TOKEN", "hvs.ADMIN-SECRET-TOKEN")
	addVault := func(file []byte) []byte { return sopstest.AddVaultKey(t, file, srv.URL) }

	// Only age is trusted unless the workstation says otherwise, so the
	// file is refused before any key is tried, and says why.
	dir, _ := secretSite{values: bmcSecret, identities: true, edit: addVault}.write(t)
	h, err := run(t, harnessOptions{config: []string{dir}, tty: true}, "secrets", "check", "--decrypt")
	if exitcode.From(err) != exitcode.TargetFailed {
		t.Fatalf("secrets check --decrypt: err = %v, want the file refused\n%s", err, h.out)
	}
	for _, want := range []string{"hc_vault", "sopsKeyTypes"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, h.out)
		}
	}
	h, err = run(t, harnessOptions{config: []string{dir}, tty: true}, "secrets", "check")
	if exitcode.From(err) != exitcode.TargetFailed || !strings.Contains(h.out.String(), "hc_vault") {
		t.Errorf("secrets check should report the key type without decrypting: %v\n%s", err, h.out)
	}

	// A workstation that trusts Vault as well still tries its age identity
	// first, and needs nothing else.
	dir, _ = secretSite{
		values: bmcSecret, identities: true, edit: addVault,
		workstation: "  sopsKeyTypes: [age, hc_vault]\n",
	}.write(t)
	h, err = run(t, harnessOptions{config: []string{dir}, tty: true}, "secrets", "check", "--decrypt")
	if err != nil {
		t.Fatalf("secrets check --decrypt with Vault trusted failed: %v\n%s", err, h.out)
	}

	if n := requests.Load(); n != 0 {
		t.Errorf("the Vault named by the file received %d requests", n)
	}
}

// TestSecretsWithoutATerminalUseOnlyTheWorkstationIdentities is report
// section 10.6: without a terminal, as under MCP, sops' own key discovery ran
// SOPS_AGE_KEY_CMD and asked gpg-agent for a passphrase, with nobody there
// to have asked for it.
func TestSecretsWithoutATerminalUseOnlyTheWorkstationIdentities(t *testing.T) {
	dir, keyFile := secretSite{values: bmcSecret}.write(t)
	marker := filepath.Join(t.TempDir(), "ran")
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\ntouch "+marker+"\ncat "+keyFile+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_CMD", probe)
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	t.Setenv("HOME", t.TempDir())

	h, err := run(t, harnessOptions{config: []string{dir}}, "secrets", "check", "--decrypt")
	if exitcode.From(err) != exitcode.TargetFailed {
		t.Fatalf("secrets check --decrypt without a terminal: err = %v, want a refusal\n%s", err, h.out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("SOPS_AGE_KEY_CMD ran without a terminal")
	}
	for _, want := range []string{"workstation.identities", "terminal"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, h.out)
		}
	}
	if strings.Contains(h.out.String(), keyFile) {
		t.Errorf("the refusal names a key file sops found:\n%s", h.out)
	}

	// At a terminal, sops looks for a key itself, as before.
	h, err = run(t, harnessOptions{config: []string{dir}, tty: true}, "secrets", "check", "--decrypt")
	if err != nil {
		t.Fatalf("secrets check --decrypt at a terminal failed: %v\n%s", err, h.out)
	}
}

// TestSecretsRefuseAMACOverEncryptedValuesOnly is report section 8.7: with
// mac_only_encrypted the message authentication code does not cover the
// kind and the name, so they could be edited without a key.
func TestSecretsRefuseAMACOverEncryptedValuesOnly(t *testing.T) {
	dir, _ := secretSite{values: bmcSecret, identities: true, opts: sopstest.Options{MACOnlyEncrypted: true}}.write(t)
	_, err := run(t, harnessOptions{config: []string{dir}}, "config", "validate")
	if got := exitcode.From(err); got != exitcode.Usage || !strings.Contains(err.Error(), "mac_only_encrypted") {
		t.Fatalf("config validate: exit %d (%v), want the file refused", got, err)
	}
}

// leaked reports whether anything a command printed or returned holds the
// plaintext of the test secrets.
func leaked(h *harness, err error) bool {
	text := h.out.String() + h.errOut.String()
	if err != nil {
		text += err.Error()
	}
	return strings.Contains(text, "hunter2") || strings.Contains(text, "czNjcjN0")
}
