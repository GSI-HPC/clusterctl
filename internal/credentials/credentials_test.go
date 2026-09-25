// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package credentials_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/credentials"
)

func resolver(t *testing.T, creds map[string]v1alpha1.Credential, env map[string]string) *credentials.Resolver {
	t.Helper()
	return &credentials.Resolver{
		Credentials: creds,
		BaseDir:     t.TempDir(),
		Env:         func(k string) string { return env[k] },
		Prompt:      func(string) (string, error) { return "typed", nil },
	}
}

func TestFromEnvironment(t *testing.T) {
	t.Parallel()

	r := resolver(t, map[string]v1alpha1.Credential{
		"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{FromEnv: "BMC_PASSWORD"}},
	}, map[string]string{"BMC_PASSWORD": "hunter2"})

	cred, err := r.Get(context.Background(), "bmc")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got, want := cred.Username, "admin"; got != want {
		t.Errorf("username = %q, want %q", got, want)
	}
	if got, want := cred.Password(), "hunter2"; got != want {
		t.Errorf("password = %q, want %q", got, want)
	}
}

func TestPasswordDoesNotLeakIntoOutput(t *testing.T) {
	t.Parallel()

	r := resolver(t, map[string]v1alpha1.Credential{
		"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{FromEnv: "BMC_PASSWORD"}},
	}, map[string]string{"BMC_PASSWORD": "hunter2"})

	cred, err := r.Get(context.Background(), "bmc")
	if err != nil {
		t.Fatal(err)
	}
	// A credential that is logged or formatted by accident must not carry
	// the password with it.
	for _, rendered := range []string{cred.String(), fmt.Sprintf("%v", cred), fmt.Sprintf("%+v", cred)} {
		if strings.Contains(rendered, "hunter2") {
			t.Errorf("the password appeared in %q", rendered)
		}
	}
}

func TestFromFileAndCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte("from-file\nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &credentials.Resolver{
		BaseDir: dir,
		Credentials: map[string]v1alpha1.Credential{
			"file":    {Username: "u", Password: v1alpha1.PasswordSource{File: "password"}},
			"command": {Username: "u", Password: v1alpha1.PasswordSource{Command: []string{"echo", "from-command"}}},
			"prompt":  {Username: "u", Password: v1alpha1.PasswordSource{Prompt: true}},
		},
		Env:    func(string) string { return "" },
		Prompt: func(string) (string, error) { return "typed", nil },
	}

	for name, want := range map[string]string{
		"file": "from-file", "command": "from-command", "prompt": "typed",
	} {
		cred, err := r.Get(context.Background(), name)
		if err != nil {
			t.Errorf("Get(%q) failed: %v", name, err)
			continue
		}
		if got := cred.Password(); got != want {
			t.Errorf("Get(%q) password = %q, want %q", name, got, want)
		}
	}
}

func TestExactlyOneSourceIsRequired(t *testing.T) {
	t.Parallel()

	r := resolver(t, map[string]v1alpha1.Credential{
		"none": {Username: "u"},
		"two": {Username: "u", Password: v1alpha1.PasswordSource{
			FromEnv: "A", File: "b",
		}},
	}, nil)

	for name, want := range map[string]string{
		"none": "nowhere to read",
		"two":  "exactly one",
	} {
		_, err := r.Get(context.Background(), name)
		if err == nil {
			t.Errorf("Get(%q) should fail", name)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Get(%q) = %v, want it to mention %q", name, err, want)
		}
	}
}

func TestUnknownCredentialListsTheKnownOnes(t *testing.T) {
	t.Parallel()

	r := resolver(t, map[string]v1alpha1.Credential{
		"pdu":  {Username: "admin", Password: v1alpha1.PasswordSource{FromEnv: "X"}},
		"bmc":  {Username: "admin", Password: v1alpha1.PasswordSource{FromEnv: "X"}},
		"ipmi": {Username: "admin", Password: v1alpha1.PasswordSource{FromEnv: "X"}},
	}, map[string]string{"X": "y"})

	_, err := r.Get(context.Background(), "nope")
	if err == nil {
		t.Fatal("an unknown credential should be reported")
	}
	// In sorted order, so that the message is the same every time.
	if !strings.Contains(err.Error(), "defines bmc, ipmi, pdu") {
		t.Errorf("error = %v, want it to list the known credentials in order", err)
	}
	if _, err := r.Get(context.Background(), ""); err == nil {
		t.Error("an empty name should be reported")
	}
}

func TestUnsetEnvironmentIsReported(t *testing.T) {
	t.Parallel()

	r := resolver(t, map[string]v1alpha1.Credential{
		"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{FromEnv: "BMC_PASSWORD"}},
	}, nil)

	_, err := r.Get(context.Background(), "bmc")
	if err == nil {
		t.Fatal("an unset variable should be reported")
	}
	if !strings.Contains(err.Error(), "BMC_PASSWORD") {
		t.Errorf("error = %v, want it to name the variable", err)
	}
}

func TestPasswordIsReadOncePerProcess(t *testing.T) {
	t.Parallel()

	asked := 0
	r := &credentials.Resolver{
		Credentials: map[string]v1alpha1.Credential{
			"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{Prompt: true}},
		},
		Env: func(string) string { return "" },
		Prompt: func(string) (string, error) {
			asked++
			return "typed", nil
		},
	}
	for i := 0; i < 3; i++ {
		if _, err := r.Get(context.Background(), "bmc"); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 1 {
		t.Errorf("the password was asked for %d times, want once", asked)
	}
}

func TestFromSecretRef(t *testing.T) {
	t.Parallel()

	asked := 0
	r := &credentials.Resolver{
		Credentials: map[string]v1alpha1.Credential{
			"bmc":   {Username: "admin", Password: v1alpha1.PasswordSource{SecretRef: &v1alpha1.SecretKeyRef{Name: "vault", Key: "bmc"}}},
			"empty": {Username: "admin", Password: v1alpha1.PasswordSource{SecretRef: &v1alpha1.SecretKeyRef{Name: "vault", Key: "empty"}}},
			"two":   {Username: "admin", Password: v1alpha1.PasswordSource{SecretRef: &v1alpha1.SecretKeyRef{Name: "vault", Key: "bmc"}, FromEnv: "X"}},
		},
		Env: func(string) string { return "" },
		Secret: func(ref v1alpha1.SecretKeyRef) ([]byte, error) {
			asked++
			return map[string][]byte{"bmc": []byte("hunter2\n"), "empty": []byte("\n")}[ref.Key], nil
		},
	}

	cred, err := r.Get(context.Background(), "bmc")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	// A block scalar leaves a newline behind; a password has none.
	if got, want := cred.Password(), "hunter2"; got != want {
		t.Errorf("password = %q, want %q", got, want)
	}
	if _, err := r.Get(context.Background(), "empty"); err == nil {
		t.Error("an empty value should be refused")
	}
	if _, err := r.Get(context.Background(), "two"); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("secretRef beside another source should be refused, got %v", err)
	}
	if asked != 2 {
		t.Errorf("the Secret was read %d times, want 2", asked)
	}

	r.Secret = nil
	r.Credentials["late"] = r.Credentials["bmc"]
	if _, err := r.Get(context.Background(), "late"); err == nil {
		t.Error("a secretRef without a Secret reader should be reported")
	}
}

// Get promises one read per process, and that has to hold when the BMC
// commands fan out: eight concurrent prompts saved and restored each other's
// terminal modes, and a helper ran eight times.
func TestConcurrentLookupsReadOnce(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		asked int
	)
	r := &credentials.Resolver{
		Credentials: map[string]v1alpha1.Credential{
			"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{Prompt: true}},
		},
		Env: func(string) string { return "" },
		Prompt: func(string) (string, error) {
			mu.Lock()
			asked++
			mu.Unlock()
			// Long enough for every other lookup to arrive meanwhile.
			time.Sleep(20 * time.Millisecond)
			return "typed", nil
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cred, err := r.Get(context.Background(), "bmc")
			if err != nil || cred.Password() != "typed" {
				t.Errorf("Get = %v, %v", cred, err)
			}
		}()
	}
	wg.Wait()
	if asked != 1 {
		t.Errorf("prompted %d times for 8 concurrent lookups, want once", asked)
	}
}

// A relative helper resolves against the site directory, as a relative file
// does. It ran relative to the working directory, where anyone who could
// plant that path chose the password sent to the service processors.
func TestRelativeHelperResolvesAgainstTheSite(t *testing.T) {
	site := t.TempDir()
	cwd := t.TempDir()
	for dir, answer := range map[string]string{site: "from-config-dir", cwd: "from-cwd"} {
		if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\necho " + answer + "\n"
		if err := os.WriteFile(filepath.Join(dir, "scripts", "bmc-password"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scripts", "password"), []byte(answer+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(cwd)

	r := &credentials.Resolver{
		Credentials: map[string]v1alpha1.Credential{
			"command": {Username: "admin", Password: v1alpha1.PasswordSource{Command: []string{"scripts/bmc-password"}}},
			"dotted":  {Username: "admin", Password: v1alpha1.PasswordSource{Command: []string{"./scripts/bmc-password"}}},
			"file":    {Username: "admin", Password: v1alpha1.PasswordSource{File: "scripts/password"}},
			// A bare name is still looked up in PATH.
			"bare": {Username: "admin", Password: v1alpha1.PasswordSource{Command: []string{"echo", "from-path"}}},
		},
		BaseDir: site,
		Env:     func(string) string { return "" },
	}
	for name, want := range map[string]string{
		"command": "from-config-dir",
		"dotted":  "from-config-dir",
		"file":    "from-config-dir",
		"bare":    "from-path",
	} {
		cred, err := r.Get(context.Background(), name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := cred.Password(); got != want {
			t.Errorf("%s resolved to %q, want %q", name, got, want)
		}
	}
}

// TestHelperHasNoTerminalWithoutOne checks that without a terminal a
// password helper runs in a session of its own, so that a helper such as
// gpg's pinentry cannot put a prompt on the terminal an MCP client runs in.
func TestHelperHasNoTerminalWithoutOne(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc to read the session from")
	}
	// The helper prints whether it leads its own session.
	script := `read -r pid _ _ _ _ sid _ < /proc/$$/stat; [ "$sid" = "$pid" ] && echo own || echo shared`
	for _, tc := range []struct {
		noTerminal bool
		want       string
	}{{true, "own"}, {false, "shared"}} {
		r := resolver(t, map[string]v1alpha1.Credential{
			"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{Command: []string{"sh", "-c", script}}},
		}, nil)
		r.NoTerminal = tc.noTerminal
		cred, err := r.Get(context.Background(), "bmc")
		if err != nil {
			t.Fatal(err)
		}
		if cred.Password() != tc.want {
			t.Errorf("NoTerminal %v: the helper's session is %q, want %q", tc.noTerminal, cred.Password(), tc.want)
		}
	}
}
