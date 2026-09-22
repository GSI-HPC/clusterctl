// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"fmt"
	"io"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/secrets"
)

// maxSecretSize bounds what "secrets encrypt" reads from standard input. A
// key or keytab is a few kilobytes; anything near this is a wrong pipe.
const maxSecretSize = 1 << 20

// Identities loads the configured age identities.
func (a *App) Identities() ([]age.Identity, error) {
	ids, err := secrets.Identities(a.IdentityPaths())
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	return ids, nil
}

// SecretContent decrypts one secret file of the site into memory, from its
// own file or from the ciphertext written inline.
func (a *App) SecretContent(file v1alpha1.SecretFile, identities []age.Identity) ([]byte, error) {
	switch {
	case file.Source != "" && file.Age != "":
		return nil, exitcode.Errorf(exitcode.Usage, "secret %s names both source and age; exactly one is allowed", file.Target)
	case file.Age != "":
		out, err := secrets.DecryptArmored(file.Age, identities)
		if err != nil {
			return nil, exitcode.Wrap(exitcode.Usage, fmt.Errorf("secret %s: %w", file.Target, err))
		}
		return out, nil
	case file.Source != "":
		out, err := secrets.Decrypt(a.Path(file.Source), identities)
		if err != nil {
			return nil, exitcode.Wrap(exitcode.Usage, err)
		}
		return out, nil
	default:
		return nil, exitcode.Errorf(exitcode.Usage, "secret %s names neither source nor age", file.Target)
	}
}

// ReadSecret reads a value to be encrypted: from the terminal without echo,
// asked twice so that a typo is not sealed away, or from standard input when
// it is not a terminal.
func (a *App) ReadSecret(prompt string) ([]byte, error) {
	if !a.IsTTY {
		data, err := io.ReadAll(io.LimitReader(a.In, maxSecretSize+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxSecretSize {
			return nil, exitcode.Errorf(exitcode.Usage, "standard input is larger than %d bytes; pass the file with --file", maxSecretSize)
		}
		return data, nil
	}
	first, err := a.promptPassword(prompt + ": ")
	if err != nil {
		return nil, err
	}
	second, err := a.promptPassword("Again: ")
	if err != nil {
		return nil, err
	}
	if first != second {
		return nil, exitcode.Errorf(exitcode.Usage, "the two entries differ")
	}
	return []byte(first), nil
}
