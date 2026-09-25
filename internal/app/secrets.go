// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/secrets"
)

// secretStore holds what this process has decrypted, so that a command that
// needs several keys of one Secret document asks for the key once.
type secretStore struct {
	mu     sync.Mutex
	ids    []secrets.IdentityFile
	idsErr error
	idsSet bool
	values map[string]map[string][]byte
	sops   *secrets.Sops
}

// Identities loads the configured age identities, once.
func (a *App) Identities() ([]age.Identity, error) {
	s := &a.secrets
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := a.identityFilesLocked()
	if err != nil {
		return nil, err
	}
	return secrets.AgeIdentities(files), nil
}

// identityFilesLocked reads workstation.identities once, for both kinds of
// secret: an age encrypted file is opened with the identities they hold, a
// Secret document by sops, which is given their paths.
func (a *App) identityFilesLocked() ([]secrets.IdentityFile, error) {
	s := &a.secrets
	if !s.idsSet {
		s.ids, s.idsErr = secrets.IdentityFiles(a.IdentityPaths())
		if s.idsErr != nil {
			s.idsErr = exitcode.Wrap(exitcode.Usage, s.idsErr)
		}
		s.idsSet = true
	}
	return s.ids, s.idsErr
}

// SecretValue decrypts the Secret document a reference names, once per
// process, and returns the value of the key. The plaintext stays in memory.
func (a *App) SecretValue(ref v1alpha1.SecretKeyRef) ([]byte, error) {
	values, err := a.SecretValues(ref.Name)
	if err != nil {
		return nil, err
	}
	value, ok := values[ref.Key]
	if !ok {
		// The keys are checked when the configuration loads, so this is a
		// file that changed since.
		return nil, exitcode.Errorf(exitcode.Usage, "the Secret %q has no key %q", ref.Name, ref.Key)
	}
	return value, nil
}

// SecretValues decrypts a whole Secret document.
func (a *App) SecretValues(name string) (map[string][]byte, error) {
	s := &a.secrets
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.values[name]; ok {
		return v, nil
	}
	doc, ok := a.Resolved.Bundle.Secrets[name]
	if !ok {
		return nil, exitcode.Errorf(exitcode.Usage, "no Secret document is named %q", name)
	}
	raw, err := os.ReadFile(doc.File)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	// Without workstation.identities sops looks for a key itself, so a
	// missing identity is not an error here. It does so only at a
	// terminal: it can run SOPS_AGE_KEY_CMD or have gpg-agent ask for a
	// passphrase, which under MCP or in a script nobody asked for.
	keys := secrets.SopsKeys{Discover: a.IsTTY, Types: a.Spec.Workstation.SopsKeyTypes}
	if len(a.Spec.Workstation.Identities) > 0 {
		if keys.Identities, err = a.identityFilesLocked(); err != nil {
			return nil, err
		}
	}
	sections, err := secrets.DecryptSops(a.Context(), a.sopsLocked(), raw, keys, config.SecretSections())
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, fmt.Errorf("the Secret %q (%s): %w", name, doc.File, err))
	}
	values, err := config.SecretValues(doc.File, sections)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if s.values == nil {
		s.values = map[string]map[string][]byte{}
	}
	s.values[name] = values
	return values, nil
}

// Sops is the sops command Secret documents are decrypted with:
// workstation.sopsBinary, or sops in PATH.
func (a *App) Sops() *secrets.Sops {
	a.secrets.mu.Lock()
	defer a.secrets.mu.Unlock()
	return a.sopsLocked()
}

func (a *App) sopsLocked() *secrets.Sops {
	s := &a.secrets
	if s.sops == nil {
		// A binary named by a path resolves against the site, as a
		// credential helper does; a bare name is looked up in PATH.
		binary := a.Spec.Workstation.SopsBinary
		if strings.ContainsRune(binary, '/') {
			binary = a.Path(binary)
		}
		s.sops = &secrets.Sops{Binary: binary}
	}
	return s.sops
}

// SecretContent decrypts one secret file of the site into memory, from its
// own age encrypted file or from a key of a Secret document.
func (a *App) SecretContent(file v1alpha1.SecretFile) ([]byte, error) {
	if file.SecretRef != nil {
		return a.SecretValue(*file.SecretRef)
	}
	return a.AgeFile(file.Source)
}

// AgeFile decrypts an age encrypted file of the site into memory, with the
// identities of the workstation.
func (a *App) AgeFile(path string) ([]byte, error) {
	ids, err := a.Identities()
	if err != nil {
		return nil, err
	}
	out, err := secrets.Decrypt(a.Path(path), ids)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	return out, nil
}
