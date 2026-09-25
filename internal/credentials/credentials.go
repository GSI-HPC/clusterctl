// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package credentials resolves the accounts used for service processors and
// power distribution units.
//
// A password is never written in the configuration, only where to read it
// from: an environment variable, a file, an age encrypted file, a key of a
// sops encrypted Secret document, a helper command or the terminal. Once resolved it is passed to a backend over a
// file or standard input, never in an argument vector where ps would show it
// to everyone on the host.
package credentials

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
)

// Credential is a resolved account.
type Credential struct {
	Name     string
	Username string
	password string
}

// Password returns the secret. It is deliberately a method rather than a
// field, so that a credential printed with %v or serialised into output does
// not carry the password with it.
func (c Credential) Password() string { return c.password }

// String hides the password from logs and error messages.
func (c Credential) String() string {
	return fmt.Sprintf("credential %s (user %s)", c.Name, c.Username)
}

// Resolver reads the credentials of a site.
type Resolver struct {
	// Credentials are the configured entries.
	Credentials map[string]v1alpha1.Credential
	// Path resolves a file the configuration names against the site; nil
	// takes the name as it is.
	Path func(string) string
	// AgeFile decrypts an age encrypted file, named as the configuration
	// names it, for an ageFile source.
	AgeFile func(path string) ([]byte, error)
	// Env reads environment variables; nil reads the process environment.
	Env func(string) string
	// Prompt asks the administrator for a password.
	Prompt func(prompt string) (string, error)
	// Secret reads a key of a Secret document for a secretRef source.
	Secret func(ref v1alpha1.SecretKeyRef) ([]byte, error)
	// NoTerminal says that nobody is at a terminal to answer a prompt, as
	// under the MCP server. A command source then runs in a session of its
	// own, so that a helper such as gpg's pinentry cannot ask on whatever
	// terminal the process was started from.
	NoTerminal bool
	// Stderr receives what a command source writes on its standard error;
	// nil is the process's.
	Stderr io.Writer

	// reading is held across a whole lookup, so that concurrent callers
	// wait for the one read instead of each prompting or running the
	// helper. Reads are rare and two prompts must not share a terminal, so
	// one lock for every name is enough.
	reading sync.Mutex
	mu      sync.Mutex
	cache   map[string]Credential
}

// Get resolves a credential by name, reading its password once per process.
// It is safe for concurrent use: a caller that arrives while the password is
// being read waits for that read.
func (r *Resolver) Get(ctx context.Context, name string) (Credential, error) {
	if name == "" {
		return Credential{}, fmt.Errorf("no credential was named")
	}
	r.reading.Lock()
	defer r.reading.Unlock()

	r.mu.Lock()
	if cached, ok := r.cache[name]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	spec, ok := r.Credentials[name]
	if !ok {
		return Credential{}, fmt.Errorf("unknown credential %q; the site defines %s",
			name, strings.Join(slices.Sorted(maps.Keys(r.Credentials)), ", "))
	}
	password, err := r.read(ctx, name, spec.Password)
	if err != nil {
		return Credential{}, err
	}
	out := Credential{Name: name, Username: spec.Username, password: password}

	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]Credential{}
	}
	r.cache[name] = out
	r.mu.Unlock()
	return out, nil
}

func (r *Resolver) read(ctx context.Context, name string, src v1alpha1.PasswordSource) (string, error) {
	sources := 0
	for _, set := range []bool{src.FromEnv != "", src.File != "", src.AgeFile != "", src.SecretRef != nil, len(src.Command) > 0, src.Prompt} {
		if set {
			sources++
		}
	}
	switch sources {
	case 0:
		return "", fmt.Errorf("credential %q says nowhere to read its password from", name)
	case 1:
	default:
		return "", fmt.Errorf("credential %q names %d password sources; exactly one is allowed", name, sources)
	}

	env := r.Env
	if env == nil {
		env = os.Getenv
	}

	switch {
	case src.FromEnv != "":
		value := env(src.FromEnv)
		if value == "" {
			return "", fmt.Errorf("credential %q reads %s, which is not set", name, src.FromEnv)
		}
		return value, nil

	case src.File != "":
		data, err := os.ReadFile(r.path(src.File))
		if err != nil {
			return "", fmt.Errorf("credential %q: %w", name, err)
		}
		return firstLine(string(data)), nil

	case src.AgeFile != "":
		data, err := r.AgeFile(src.AgeFile)
		if err != nil {
			return "", fmt.Errorf("credential %q: %w", name, err)
		}
		// The file holds one line, and its newline is not part of it.
		return strings.TrimRight(string(data), "\r\n"), nil

	case src.SecretRef != nil:
		if r.Secret == nil {
			return "", fmt.Errorf("credential %q reads Secret %s, but no Secret documents were loaded", name, src.SecretRef)
		}
		data, err := r.Secret(*src.SecretRef)
		if err != nil {
			return "", fmt.Errorf("credential %q: %w", name, err)
		}
		value := strings.TrimRight(string(data), "\r\n")
		if value == "" {
			return "", fmt.Errorf("credential %q: Secret %s is empty", name, src.SecretRef)
		}
		return value, nil

	case len(src.Command) > 0:
		// A helper named by a path resolves against the site, as a file
		// does; relative to the working directory it would be whatever
		// the directory clusterctl was started in holds. A bare name is
		// looked up in PATH.
		helper := src.Command[0]
		if strings.ContainsRune(helper, '/') {
			helper = r.path(helper)
		}
		cmd := exec.CommandContext(ctx, helper, src.Command[1:]...)
		cmd.Stderr = r.Stderr
		if cmd.Stderr == nil {
			cmd.Stderr = os.Stderr
		}
		// A helper that leaves something behind holding its output does
		// not keep the lookup waiting once the context has ended.
		cmd.WaitDelay = 5 * time.Second
		if r.NoTerminal {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("credential %q: the helper failed: %w", name, err)
		}
		return firstLine(string(out)), nil

	default:
		if r.Prompt == nil {
			return "", fmt.Errorf("credential %q asks on the terminal, but there is none", name)
		}
		value, err := r.Prompt(fmt.Sprintf("Password for %s@%s: ", r.Credentials[name].Username, name))
		if err != nil {
			return "", err
		}
		if value == "" {
			return "", fmt.Errorf("credential %q: no password was given", name)
		}
		return value, nil
	}
}

func (r *Resolver) path(p string) string {
	if r.Path == nil {
		return p
	}
	return r.Path(p)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
