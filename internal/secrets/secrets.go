// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package secrets decrypts the age encrypted files a site keeps beside its
// configuration, and has the sops command decrypt its Secret documents.
//
// Plaintext is returned in memory and streamed to wherever it is needed. It
// is never written to the workstation's disk, which is what the exec wrappers
// of the shell toolkit were careful about and what this package keeps.
package secrets

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"
)

// Identities loads the age identities used to decrypt.
//
// A file may hold age identities or an OpenSSH private key; both are
// accepted, because a site usually already has ssh keys and no reason to
// issue a second kind.
func Identities(paths []string) ([]age.Identity, error) {
	var out []age.Identity
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading the identity %s: %w", path, err)
		}
		ids, err := parseIdentities(path, data)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no age identity is configured; set workstation.identities")
	}
	return out, nil
}

func parseIdentities(path string, data []byte) ([]age.Identity, error) {
	if isSSHKey(data) {
		id, err := agessh.ParseIdentity(data)
		if err != nil {
			return nil, fmt.Errorf("reading the ssh identity %s: %w", path, err)
		}
		return []age.Identity{id}, nil
	}
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reading the age identity %s: %w", path, err)
	}
	return ids, nil
}

// IdentityFile is one file of workstation.identities, as sops is given it:
// by its path, which sops opens itself, so that no key is copied anywhere.
type IdentityFile struct {
	// Path is the file, absolute, since sops runs in another directory.
	Path string
	// SSH is set for an OpenSSH private key, which sops reads from another
	// variable than age identities.
	SSH bool
	// recipients are the recipients the file's identities open, in the
	// form recipientKey gives them.
	recipients []string
}

// IdentityFiles reads the identities of workstation.identities and notes
// which recipients each file opens. A file is read and checked here, the
// way Identities reads it, before sops is pointed at it: sops would prompt
// for the passphrase of an encrypted key, or run the plugin an identity
// names.
func IdentityFiles(paths []string) ([]IdentityFile, error) {
	out := make([]IdentityFile, 0, len(paths))
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("reading the identity %s: %w", path, err)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("reading the identity %s: %w", path, err)
		}
		ids, err := parseIdentities(path, data)
		if err != nil {
			return nil, err
		}
		f := IdentityFile{Path: abs, SSH: isSSHKey(data)}
		if f.SSH {
			signer, err := ssh.ParsePrivateKey(data)
			if err != nil {
				return nil, fmt.Errorf("reading the ssh identity %s: %w", path, err)
			}
			f.recipients = []string{sshRecipientKey(signer.PublicKey())}
		}
		for _, id := range ids {
			switch id := id.(type) {
			case *age.X25519Identity:
				f.recipients = append(f.recipients, id.Recipient().String())
			case *age.HybridIdentity:
				f.recipients = append(f.recipients, id.Recipient().String())
			}
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no age identity is configured; set workstation.identities")
	}
	return out, nil
}

// Opens reports whether the file holds the identity of an age recipient,
// as sops writes one into its metadata.
func (f IdentityFile) Opens(recipient string) bool {
	key := recipientKey(recipient)
	return key != "" && slices.Contains(f.recipients, key)
}

// recipientKey is the form two spellings of one recipient share: an age
// recipient as written, an OpenSSH public key without its comment.
func recipientKey(recipient string) string {
	recipient = strings.TrimSpace(recipient)
	if strings.HasPrefix(recipient, "ssh-") {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(recipient))
		if err != nil {
			return ""
		}
		return sshRecipientKey(pub)
	}
	return recipient
}

func sshRecipientKey(pub ssh.PublicKey) string {
	return "ssh:" + base64.StdEncoding.EncodeToString(pub.Marshal())
}

func isSSHKey(data []byte) bool {
	return bytes.Contains(data, []byte("PRIVATE KEY"))
}

// Decrypt reads an age encrypted file and returns its plaintext.
func Decrypt(path string, identities []age.Identity) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the secret %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	r, err := age.Decrypt(f, identities...)
	if err != nil {
		return nil, fmt.Errorf("decrypting %s: %w", path, err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		return nil, fmt.Errorf("decrypting %s: %w", path, err)
	}
	return out.Bytes(), nil
}

// DecryptString decrypts a file whose content is one line, such as a
// password, and returns it without the trailing newline.
func DecryptString(path string, identities []age.Identity) (string, error) {
	data, err := Decrypt(path, identities)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
