// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// withSecret builds the app over the example configuration and a second
// directory holding a Secret named example, encrypted to a fresh key, and a
// Workstation whose identities open it. Both replace the example's own.
func withSecret(t *testing.T, env map[string]string, identities bool) (*app.App, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, ".identity")
	if err := os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := sopstest.Encrypt(t, `apiVersion: clusterctl/v1alpha1
kind: Secret
metadata:
  name: example
data:
  bmc-password: hunter2
binaryData:
  munge-key: AAECAwQ=
`, id.Recipient().String())
	ws := "apiVersion: clusterctl/v1alpha1\nkind: Workstation\nspec:\n  host: test\n"
	if identities {
		ws += "  identities:\n    - " + keyFile + "\n"
	}
	for name, content := range map[string]string{"secrets.sops.yaml": string(secret), "workstation.yaml": ws} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	a, err := app.New(context.Background(), app.Streams{
		In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{},
		StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"),
	}, app.Options{
		ConfigFiles: []string{exampleDir, dir},
		Env:         func(k string) string { return env[k] },
		Runner:      &transport.Recorder{},
	})
	if err != nil {
		t.Fatalf("building the app: %v", err)
	}
	return a, keyFile
}

func TestSecretValueDecryptsWithTheWorkstationIdentities(t *testing.T) {
	a, _ := withSecret(t, nil, true)

	got, err := a.SecretValue(v1alpha1.SecretKeyRef{Name: "example", Key: "munge-key"})
	if err != nil {
		t.Fatalf("SecretValue failed: %v", err)
	}
	if string(got) != "\x00\x01\x02\x03\x04" {
		t.Errorf("munge-key = %q, want the decoded bytes", got)
	}

	// The example site's bmc-sops credential refers to the same document.
	cred, err := a.Credentials().Get(context.Background(), "bmc-sops")
	if err != nil {
		t.Fatalf("resolving the credential: %v", err)
	}
	if cred.Password() != "hunter2" {
		t.Errorf("password = %q, want hunter2", cred.Password())
	}

	content, err := a.SecretContent(a.Spec.Services.Cinc.Secrets[0])
	if err != nil || len(content) != 5 {
		t.Errorf("SecretContent = %q, %v; want the munge key", content, err)
	}
}

func TestSecretValueFallsBackToTheKeysOfSops(t *testing.T) {
	// No workstation.identities: sops finds the key itself, here through
	// SOPS_AGE_KEY_FILE. The variable is read by sops from the process
	// environment, so the test cannot run in parallel.
	a, keyFile := withSecret(t, nil, false)
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	if _, err := a.SecretValue(v1alpha1.SecretKeyRef{Name: "example", Key: "bmc-password"}); err != nil {
		t.Fatalf("sops' own key was not used: %v", err)
	}
}

func TestSecretValueSaysWhatWasTried(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", "")
	t.Setenv("HOME", t.TempDir())
	a, _ := withSecret(t, nil, false)
	_, err := a.SecretValue(v1alpha1.SecretKeyRef{Name: "example", Key: "bmc-password"})
	if err == nil {
		t.Fatal("decrypting without a key should fail")
	}
	for _, want := range []string{`Secret "example"`, "no key available here opens it", "age age1", "via sops"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
