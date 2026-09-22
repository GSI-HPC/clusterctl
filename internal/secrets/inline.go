// SPDX-License-Identifier: LGPL-3.0-or-later

package secrets

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
)

// ArmorHeader opens an ASCII armored age file. A configuration value that
// starts with it is an inline secret, wherever in a document it is written.
const ArmorHeader = armor.Header

// IsArmored reports whether a configuration value is an inline age secret.
//
// Leading whitespace is ignored, because a block scalar pasted with one more
// level of indentation than its key is still what was meant.
func IsArmored(value string) bool {
	return strings.HasPrefix(strings.TrimLeft(value, " \t\r\n"), ArmorHeader)
}

// Inspect checks that an inline secret is a complete age file without
// decrypting it, and returns the types of its recipient stanzas.
//
// No identity is needed, so the configuration can be checked on a machine
// that is not allowed to read the secret: a truncated paste, a line lost in a
// merge or a stray character is reported where it was written.
func Inspect(value string) ([]string, error) {
	raw, err := dearmor(value)
	if err != nil {
		return nil, err
	}
	return stanzaTypes(raw)
}

// InspectFile is Inspect for an age file on disk, armored or binary.
func InspectFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// The error of os.ReadFile already names the path.
		return nil, err
	}
	if IsArmored(string(data)) {
		data, err = dearmor(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	types, err := stanzaTypes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return types, nil
}

// stanzaTypes reads the header of an age file and returns the type of each
// recipient stanza, such as X25519 or ssh-ed25519.
func stanzaTypes(raw []byte) ([]string, error) {
	header, err := age.ExtractHeader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("not an age file: %w", err)
	}
	var stanzas []string
	sc := bufio.NewScanner(bytes.NewReader(header))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "-> "); ok {
			kind, _, _ := strings.Cut(rest, " ")
			stanzas = append(stanzas, kind)
		}
	}
	if len(stanzas) == 0 {
		return nil, fmt.Errorf("the age file names no recipient")
	}
	return stanzas, nil
}

// DecryptArmored decrypts an inline secret held in memory.
func DecryptArmored(value string, identities []age.Identity) ([]byte, error) {
	raw, err := dearmor(value)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(raw), identities...)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	return out.Bytes(), nil
}

// DecryptArmoredString decrypts an inline secret whose content is one line,
// such as a password, and returns it without the trailing newline.
func DecryptArmoredString(value string, identities []age.Identity) (string, error) {
	data, err := DecryptArmored(value, identities)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// EncryptArmored encrypts plaintext to the recipients and returns it ASCII
// armored, ready to be written into a document as a block scalar.
func EncryptArmored(plaintext []byte, recipients []age.Recipient) (string, error) {
	if len(recipients) == 0 {
		return "", fmt.Errorf("no recipient to encrypt to; set secrets.recipients in the site document or pass --recipient")
	}
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, recipients...)
	if err != nil {
		return "", fmt.Errorf("encrypting: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return "", fmt.Errorf("encrypting: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("encrypting: %w", err)
	}
	if err := aw.Close(); err != nil {
		return "", fmt.Errorf("encrypting: %w", err)
	}
	return buf.String(), nil
}

// ParseRecipients reads the recipients a secret is encrypted to: native age
// recipients, post-quantum hybrid ones and OpenSSH public keys, one per
// entry. Comments and blank entries are skipped.
//
// Plugin recipients are refused rather than passed through: they would run an
// external program named by the configuration.
func ParseRecipients(entries []string) ([]age.Recipient, error) {
	var out []age.Recipient
	for i, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		var (
			r   age.Recipient
			err error
		)
		switch {
		case strings.HasPrefix(entry, "ssh-"):
			r, err = agessh.ParseRecipient(entry)
		case strings.HasPrefix(entry, "age1pq1"):
			r, err = age.ParseHybridRecipient(entry)
		case strings.HasPrefix(entry, "age1"):
			// A plugin recipient such as age1yubikey1... fails here, with
			// a message naming the prefix it did not expect.
			r, err = age.ParseX25519Recipient(entry)
		default:
			// The entry may be a private key pasted by mistake, so it is
			// not repeated in the message.
			err = fmt.Errorf("not an age or ssh public key")
		}
		if err != nil {
			return nil, fmt.Errorf("recipient %d: %w", i+1, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// dearmor reads the ASCII armor of an inline secret, which must be the whole
// value: text before or after the armor is refused, so that a value is
// either a secret or not one.
func dearmor(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, ArmorHeader) {
		return nil, fmt.Errorf("an inline secret must start with %q", ArmorHeader)
	}
	if !strings.HasSuffix(trimmed, armor.Footer) {
		return nil, fmt.Errorf("the inline secret is truncated: it does not end with %q", armor.Footer)
	}
	raw, err := io.ReadAll(armor.NewReader(strings.NewReader(trimmed + "\n")))
	if err != nil {
		return nil, fmt.Errorf("the inline secret is damaged: %w", err)
	}
	return raw, nil
}
