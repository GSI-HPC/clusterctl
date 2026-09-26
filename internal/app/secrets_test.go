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
	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
	"github.com/GSI-HPC/clusterctl/internal/progress"
	"github.com/GSI-HPC/clusterctl/internal/progress/progresstest"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// withSecret builds the app over a copy of the example configuration whose
// Secret named example is encrypted to a fresh key instead, and whose
// Workstation has identities that open it. They take the place of the
// example's own, which a second document of the same name may not.
func withSecret(t *testing.T, env map[string]string, identities bool) (*app.App, string) {
	t.Helper()
	return withSecretUnder(t, context.Background(), env, identities)
}

// withSecretUnder is withSecret with the command running under ctx.
func withSecretUnder(t *testing.T, ctx context.Context, env map[string]string, identities bool) (*app.App, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := configtest.CopyDir(t, exampleDir, "secrets.sops.yaml", "workstation.yaml")
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

	// At a terminal, where sops may look for a key itself.
	a, err := app.New(ctx, app.Streams{
		In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}, IsTTY: true,
		StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"),
	}, app.Options{
		ConfigFiles: []string{dir},
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

	got, err := a.SecretValue(a.Context(), v1alpha1.SecretKeyRef{Name: "example", Key: "munge-key"})
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

	content, err := a.SecretContent(a.Context(), a.Spec.Services.Cinc.Secrets[0])
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
	if _, err := a.SecretValue(a.Context(), v1alpha1.SecretKeyRef{Name: "example", Key: "bmc-password"}); err != nil {
		t.Fatalf("sops' own key was not used: %v", err)
	}
}

func TestSecretValueSaysWhatWasTried(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", "")
	t.Setenv("HOME", t.TempDir())
	a, _ := withSecret(t, nil, false)
	_, err := a.SecretValue(a.Context(), v1alpha1.SecretKeyRef{Name: "example", Key: "bmc-password"})
	if err == nil {
		t.Fatal("decrypting without a key should fail")
	}
	// sops' own account names each key it tried: here the age recipient,
	// found by sops itself.
	for _, want := range []string{`Secret "example"`, "no key available here opens it", "age1", "sops found itself"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A decryption is a hidden call that says what decrypted it, sops for a
// Secret document and age for a file of its own, once however often its
// values are asked for, and never what it read.
func TestADecryptionIsReportedOnce(t *testing.T) {
	c := &progresstest.Capture{}
	bus := progress.NewBus(progress.Options{Sinks: []progress.Sink{c}})
	a, keyFile := withSecretUnder(t, progress.WithBus(context.Background(), bus), nil, true)
	id, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(id)))
	if err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(t.TempDir(), "munge.key.age")
	var buf strings.Builder
	w, err := age.Encrypt(&buf, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("munge"))
	_ = w.Close()
	if err := os.WriteFile(sealed, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	// Each decryption nests under what needs it, a credential read here.
	ctx, lookup := progress.Start(a.Context(), progress.KindCall, "credential bmc", progress.WithFlags(progress.Hidden))
	for _, key := range []string{"bmc-password", "munge-key"} {
		if _, err := a.SecretValue(ctx, v1alpha1.SecretKeyRef{Name: "example", Key: key}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := a.AgeFile(ctx, sealed); err != nil || string(got) != "munge" {
		t.Fatalf("AgeFile = %q, %v", got, err)
	}
	lookup.End(nil)
	bus.Close()
	events := c.Events()
	progresstest.Check(t, events)
	want := "call credential bmc [hidden]: ok\n" +
		"  call decrypt " + sealed + " source=age [hidden]: ok\n" +
		"  call decrypt the Secret example source=sops [hidden]: ok\n"
	if got := c.Tree(); got != want {
		t.Errorf("progress:\n%s\nwant:\n%s", got, want)
	}
	for _, e := range events {
		if strings.Contains(e.Name+e.Err+e.Message, "hunter2") {
			t.Errorf("event %d carries a secret: %+v", e.Seq, e)
		}
	}
}
