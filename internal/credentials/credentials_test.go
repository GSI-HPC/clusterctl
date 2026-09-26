// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package credentials_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/credentials"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

func resolver(t *testing.T, creds map[string]v1alpha1.Credential, env map[string]string) *credentials.Resolver {
	t.Helper()
	return &credentials.Resolver{
		Credentials: creds,
		Path:        inDir(t.TempDir()),
		Env:         func(k string) string { return env[k] },
		Prompt:      func(string) (string, error) { return "typed", nil },
	}
}

// inDir resolves a relative path against dir, as app.Path does against the
// site.
func inDir(dir string) func(string) string {
	return func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
}

// An age encrypted password is one line, and its newline is not part of it.
// The file is handed over as the configuration names it: app resolves it,
// as it does every other secret file.
func TestFromAnAgeFile(t *testing.T) {
	t.Parallel()

	r := resolver(t, map[string]v1alpha1.Credential{
		"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{AgeFile: "secrets/bmc.age"}},
	}, nil)
	var asked string
	r.AgeFile = func(_ context.Context, path string) ([]byte, error) {
		asked = path
		return []byte("hunter2\n"), nil
	}
	cred, err := r.Get(context.Background(), "bmc")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got := cred.Password(); got != "hunter2" {
		t.Errorf("password = %q, want hunter2", got)
	}
	if asked != "secrets/bmc.age" {
		t.Errorf("decrypted %q, want the path as configured", asked)
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
		Path: inDir(dir),
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
	for range 3 {
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
		Secret: func(_ context.Context, ref v1alpha1.SecretKeyRef) ([]byte, error) {
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
	for range 8 {
		wg.Go(func() {
			cred, err := r.Get(context.Background(), "bmc")
			if err != nil || cred.Password() != "typed" {
				t.Errorf("Get = %v, %v", cred, err)
			}
		})
	}
	wg.Wait()
	if asked != 1 {
		t.Errorf("prompted %d times for 8 concurrent lookups, want once", asked)
	}
}

// A read that failed was not remembered, and provision status builds a
// client for every node and goes on after an error: a helper that failed ran
// once per node, and an empty answer at the prompt was asked for again for
// each. Every caller gets the one failure now, however many ask at once.
func TestAFailedReadIsMadeOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		src  v1alpha1.PasswordSource
		// fail makes the source fail, calling read each time it is read.
		fail func(r *credentials.Resolver, read func())
	}{
		{"a prompt answered with nothing", v1alpha1.PasswordSource{Prompt: true},
			func(r *credentials.Resolver, read func()) {
				r.Prompt = func(string) (string, error) { read(); return "", nil }
			}},
		{"a Secret that cannot be read", v1alpha1.PasswordSource{SecretRef: &v1alpha1.SecretKeyRef{Name: "vault", Key: "bmc"}},
			func(r *credentials.Resolver, read func()) {
				r.Secret = func(context.Context, v1alpha1.SecretKeyRef) ([]byte, error) {
					read()
					return nil, errors.New("no identity opens it")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var (
				mu    sync.Mutex
				reads int
			)
			r := resolver(t, map[string]v1alpha1.Credential{"bmc": {Username: "admin", Password: tc.src}}, nil)
			tc.fail(r, func() {
				mu.Lock()
				reads++
				mu.Unlock()
				// Long enough for every other lookup to arrive meanwhile.
				time.Sleep(20 * time.Millisecond)
			})

			errs := make([]error, 8)
			var wg sync.WaitGroup
			for i := range errs {
				wg.Go(func() { _, errs[i] = r.Get(context.Background(), "bmc") })
			}
			wg.Wait()
			for i, err := range errs {
				if err == nil {
					t.Errorf("lookup %d succeeded although the source failed", i)
				}
			}
			if _, err := r.Get(context.Background(), "bmc"); err == nil || err.Error() != errs[0].Error() {
				t.Errorf("a later lookup = %v, want the first failure again", err)
			}
			if reads != 1 {
				t.Errorf("the source was read %d times for 9 lookups, want once", reads)
			}
		})
	}
}

// A helper that fails is run once, however many lookups need it.
func TestAFailingHelperRunsOnce(t *testing.T) {
	t.Parallel()
	runs := filepath.Join(t.TempDir(), "runs")
	r := resolver(t, map[string]v1alpha1.Credential{
		"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{
			Command: []string{"sh", "-c", `echo run >> "$1"; exit 1`, "sh", runs}}},
	}, nil)
	for i := range 3 {
		if _, err := r.Get(context.Background(), "bmc"); err == nil || !strings.Contains(err.Error(), "the helper failed") {
			t.Fatalf("lookup %d = %v, want the helper's failure", i, err)
		}
	}
	data, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "run"); got != 1 {
		t.Errorf("the helper ran %d times for 3 lookups, want once", got)
	}
}

// waitingPrompt answers a prompt the way the terminal does: it waits for
// the answer, and gives up when its context ends.
type waitingPrompt struct {
	ctx    context.Context
	answer chan string
	// asking, when it is set, is sent to as the question is asked, so that
	// a test interrupts a prompt that is waiting, not one not yet asked.
	asking chan struct{}

	mu    sync.Mutex
	asked int
}

func (p *waitingPrompt) ask(string) (string, error) {
	p.mu.Lock()
	p.asked++
	ctx := p.ctx
	p.mu.Unlock()
	if p.asking != nil {
		select {
		case p.asking <- struct{}{}:
		default:
		}
	}
	select {
	case answer := <-p.answer:
		return answer, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *waitingPrompt) times() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked
}

// A Ctrl-C at the password prompt of provision status stopped nothing: the
// command went on to the next node, which asked again, and so on for every
// node, each leaving a read of the terminal behind. Once the context has
// ended nothing is read, a lookup that waited for an interrupted one reads
// nothing either, and an interrupted read is not taken for the source's
// answer.
func TestNothingIsReadOnceInterrupted(t *testing.T) {
	t.Parallel()

	promptResolver := func(t *testing.T, p *waitingPrompt) *credentials.Resolver {
		r := resolver(t, map[string]v1alpha1.Credential{
			"bmc": {Username: "admin", Password: v1alpha1.PasswordSource{Prompt: true}},
		}, nil)
		r.Prompt = p.ask
		return r
	}

	t.Run("after the interrupt", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := &waitingPrompt{ctx: ctx}
		_, err := promptResolver(t, p).Get(ctx, "bmc")
		if !errors.Is(err, context.Canceled) || exitcode.From(err) != exitcode.Interrupted {
			t.Errorf("error = %v (exit code %d), want an interrupt", err, exitcode.From(err))
		}
		if n := p.times(); n != 0 {
			t.Errorf("asked %d times after the interrupt, want never", n)
		}
	})

	t.Run("while others wait for the prompt", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := &waitingPrompt{ctx: ctx, asking: make(chan struct{}, 1)}
		r := promptResolver(t, p)
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() { _, errs[i] = r.Get(ctx, "bmc") })
		}
		// The interrupt comes while the first lookup asks; the others wait
		// for it, or come after the interrupt, and ask nothing either way.
		<-p.asking
		cancel()
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, context.Canceled) {
				t.Errorf("lookup %d = %v, want it interrupted", i, err)
			}
		}
		if n := p.times(); n != 1 {
			t.Errorf("asked %d times for 8 lookups interrupted at the prompt, want once", n)
		}
	})

	t.Run("but asked afresh once more under a live context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		p := &waitingPrompt{ctx: ctx, answer: make(chan string, 1), asking: make(chan struct{}, 1)}
		r := promptResolver(t, p)
		// The prompt is interrupted while it waits, so that its read is one
		// the failure cache has to leave out.
		go func() {
			<-p.asking
			cancel()
		}()
		if _, err := r.Get(ctx, "bmc"); !errors.Is(err, context.Canceled) {
			t.Fatalf("the interrupted lookup = %v, want it interrupted", err)
		}

		p.mu.Lock()
		p.ctx = context.Background()
		p.mu.Unlock()
		p.answer <- "typed"
		cred, err := r.Get(context.Background(), "bmc")
		if err != nil || cred.Password() != "typed" {
			t.Errorf("Get = %v, %v; want the password asked for afresh", cred, err)
		}
		if n := p.times(); n != 2 {
			t.Errorf("asked %d times, want once for the interrupted lookup and once afresh", n)
		}
	})
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
		Path: inDir(site),
		Env:  func(string) string { return "" },
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
