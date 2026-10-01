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
	"errors"
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
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/progress"
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
	AgeFile func(ctx context.Context, path string) ([]byte, error)
	// Env reads environment variables; nil reads the process environment.
	Env func(string) string
	// Prompt asks the administrator for a password. The progress displays
	// are off the terminal while it runs.
	Prompt func(prompt string) (string, error)
	// Secret reads a key of a Secret document for a secretRef source.
	Secret func(ctx context.Context, ref v1alpha1.SecretKeyRef) ([]byte, error)
	// NoTerminal says that nobody is at a terminal to answer a prompt, as
	// under the MCP server. A command source then runs in a session of its
	// own, so that a helper such as gpg's pinentry cannot ask on whatever
	// terminal the process was started from; otherwise the progress
	// displays are off the terminal while it runs.
	NoTerminal bool
	// Stderr receives what a command source writes on its standard error;
	// nil is the process's.
	Stderr io.Writer

	// terminal is held across a read that may use the terminal, so that
	// two never share it: a prompt, and, while someone is at the terminal,
	// a helper, age or sops, which may ask there for a passphrase or a PIN.
	terminal sync.Mutex
	mu       sync.Mutex
	// cache holds the credentials that were read, and failed the reads that
	// failed, by name.
	cache  map[string]Credential
	failed map[string]error
	// reading holds the reads under way, by name, each closed once its read
	// is over: a caller asking for a name being read waits for that read
	// rather than prompting or running the helper again.
	reading map[string]chan struct{}
}

// Get resolves a credential by name, reading its password once per process.
// It is safe for concurrent use: a caller that arrives while the password is
// being read waits for that read.
//
// A read that failed is remembered as well, so that a helper that fails or
// a prompt left empty is not tried again for every node the credential is
// needed for. A read the context stopped is not remembered, since it says
// nothing about the source; once ctx has ended nothing is read at all.
func (r *Resolver) Get(ctx context.Context, name string) (Credential, error) {
	if name == "" {
		return Credential{}, fmt.Errorf("no credential was named")
	}
	for {
		if err := ctx.Err(); err != nil {
			return Credential{}, notRead(name, err)
		}
		r.mu.Lock()
		if cached, ok := r.cache[name]; ok {
			r.mu.Unlock()
			return cached, nil
		}
		if failed := r.failed[name]; failed != nil {
			r.mu.Unlock()
			return Credential{}, failed
		}
		if done, ok := r.reading[name]; ok {
			r.mu.Unlock()
			// Another caller reads it; its answer is this one's too,
			// unless it was interrupted, and then it is read afresh.
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return Credential{}, notRead(name, ctx.Err())
			}
		}
		if r.reading == nil {
			r.reading = map[string]chan struct{}{}
		}
		done := make(chan struct{})
		r.reading[name] = done
		r.mu.Unlock()
		return r.readOnce(ctx, name, done)
	}
}

// readOnce reads the credential name for Get, which marked it as being read
// with done, and keeps what it found.
func (r *Resolver) readOnce(ctx context.Context, name string, done chan struct{}) (cred Credential, err error) {
	defer func() {
		r.mu.Lock()
		switch {
		case err == nil:
			if r.cache == nil {
				r.cache = map[string]Credential{}
			}
			r.cache[name] = cred
		case ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded):
			if r.failed == nil {
				r.failed = map[string]error{}
			}
			r.failed[name] = err
		}
		delete(r.reading, name)
		close(done)
		r.mu.Unlock()
	}()

	spec, ok := r.Credentials[name]
	if !ok {
		return Credential{}, fmt.Errorf("unknown credential %q; the site defines %s",
			name, strings.Join(slices.Sorted(maps.Keys(r.Credentials)), ", "))
	}
	if r.interactive(spec.Password) {
		r.terminal.Lock()
		defer r.terminal.Unlock()
		// A caller that waited for the terminal may have been interrupted
		// meanwhile.
		if err := ctx.Err(); err != nil {
			return Credential{}, notRead(name, err)
		}
	}
	// The read is reported, by the credential's name and the kind of its
	// source; what was read is not. A lookup answered from memory reads
	// nothing and is not reported.
	ctx, lookup := progress.Start(ctx, progress.KindCall, "credential "+name,
		progress.WithFlags(progress.Hidden), progress.Source(sourceKind(spec.Password)))
	password, err := r.read(ctx, name, spec.Password)
	// A password that cannot be read is the configuration's to fix, as the
	// commands report it, not a host's refusal.
	lookup.End(exitcode.Default(exitcode.Usage, err))
	if err != nil {
		return Credential{}, err
	}
	return Credential{Name: name, Username: spec.Username, password: password}, nil
}

// interactive says whether reading src may use the terminal: a prompt
// always, and a helper, age or sops while someone is at the terminal, any of
// which may ask there for a passphrase or a PIN.
func (r *Resolver) interactive(src v1alpha1.PasswordSource) bool {
	return src.Prompt || !r.NoTerminal && (len(src.Command) > 0 || src.AgeFile != "" || src.SecretRef != nil)
}

// Prefetch reads the credentials named that are not read yet side by side,
// so that a command that needs several, one for each vendor's processors,
// waits for the slowest rather than for each in turn. What it reads, or how
// a read failed, is what Get returns for the name. A credential whose read
// may use the terminal is left to Get, so that the questions come one at a
// time and in the order the command asks them.
func (r *Resolver) Prefetch(ctx context.Context, names []string) {
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, name := range names {
		spec, ok := r.Credentials[name]
		if !ok || seen[name] || r.interactive(spec.Password) {
			continue
		}
		seen[name] = true
		wg.Go(func() { _, _ = r.Get(ctx, name) })
	}
	wg.Wait()
}

// notRead is the error of a lookup that read nothing because its context
// had ended. An interrupt exits 130; a deadline is the command's failure.
func notRead(name string, err error) error {
	code := exitcode.TargetFailed
	if errors.Is(err, context.Canceled) {
		code = exitcode.Interrupted
	}
	return exitcode.Wrap(code, fmt.Errorf("credential %q was not read: %w", name, err))
}

// sourceKind names the kind of source a password is read from, for a
// display: env, file, age, sops, command or prompt, or nothing for an entry
// that names none or several.
func sourceKind(src v1alpha1.PasswordSource) string {
	kind := ""
	for _, source := range []struct {
		set  bool
		kind string
	}{
		{src.FromEnv != "", "env"},
		{src.File != "", "file"},
		{src.AgeFile != "", "age"},
		{src.SecretRef != nil, "sops"},
		{len(src.Command) > 0, "command"},
		{src.Prompt, "prompt"},
	} {
		if source.set {
			if kind != "" {
				return ""
			}
			kind = source.kind
		}
	}
	return kind
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
		path := r.path(src.File)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("credential %q: %w", name, err)
		}
		return nonEmpty(name, firstLine(string(data)), "the first line of "+path)

	case src.AgeFile != "":
		data, err := r.AgeFile(ctx, src.AgeFile)
		if err != nil {
			return "", fmt.Errorf("credential %q: %w", name, err)
		}
		// The file holds one line, and its newline is not part of it.
		return nonEmpty(name, strings.TrimRight(string(data), "\r\n"), "the age file "+src.AgeFile)

	case src.SecretRef != nil:
		if r.Secret == nil {
			return "", fmt.Errorf("credential %q reads Secret %s, but no Secret documents were loaded", name, src.SecretRef)
		}
		data, err := r.Secret(ctx, *src.SecretRef)
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
		// A helper that can reach the terminal may ask on it, as gpg's
		// pinentry does, so the displays leave it while the helper runs.
		resume := func() {}
		if !r.NoTerminal {
			resume = progress.Suspend(ctx)
		}
		out, err := cmd.Output()
		resume()
		if err != nil {
			return "", fmt.Errorf("credential %q: the helper failed: %w", name, err)
		}
		return nonEmpty(name, firstLine(string(out)), "the helper "+helper)

	default:
		if r.Prompt == nil {
			return "", fmt.Errorf("credential %q asks on the terminal, but there is none", name)
		}
		// The question is asked on the terminal, with the displays off it
		// until it has been answered.
		resume := progress.Suspend(ctx)
		value, err := r.Prompt(fmt.Sprintf("Password for %s@%s: ", r.Credentials[name].Username, name))
		resume()
		if err != nil {
			return "", err
		}
		if value == "" {
			return "", fmt.Errorf("credential %q: no password was given", name)
		}
		return value, nil
	}
}

// nonEmpty returns a password that was read, and refuses an empty one,
// naming what gave it: Redfish would send it to every service processor as
// an empty Basic authorization rather than fail.
func nonEmpty(name, password, what string) (string, error) {
	if password == "" {
		return "", exitcode.Errorf(exitcode.Usage, "credential %q: %s gave no password", name, what)
	}
	return password, nil
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
