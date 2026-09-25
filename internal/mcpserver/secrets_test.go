// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package mcpserver_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
	"github.com/GSI-HPC/clusterctl/internal/secrets/sopstest"
)

// A Secret is opened under MCP with workstation.identities alone: sops runs
// with no terminal and an environment of its own, so a key it would find
// itself, here through SOPS_AGE_KEY_FILE, opens nothing, and nothing of the
// plaintext reaches the agent either way.
func TestSecretsOpenWithTheWorkstationIdentitiesAlone(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	secret := sopstest.Encrypt(t, "apiVersion: clusterctl/v1alpha1\nkind: Secret\nmetadata:\n  name: example\n"+
		"data:\n  bmc-password: hunter2\nbinaryData:\n  munge-key: czNjcjN0LWtleQ==\n", id.Recipient().String())

	for _, tt := range []struct {
		name       string
		identities string
		exitCode   int
		want       string
	}{
		{"with the identity configured", "  identities:\n    - " + keyFile + "\n", 0, "decrypts"},
		{"without it", "", 1, "no key is available without a terminal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := configtest.CopyDir(t, exampleDir)
			if err := os.WriteFile(filepath.Join(dir, "secrets.sops.yaml"), secret, 0o600); err != nil {
				t.Fatal(err)
			}
			edit(t, dir, "workstation.yaml", "  identities:\n    - ~/.ssh/id_ed25519\n", tt.identities)
			f := start(t, setup{config: dir})

			var out commandResult
			f.call(t, "read_command", map[string]any{"args": []string{"secrets", "check", "--decrypt"}}, &out)
			output, err := json.Marshal(out.Output)
			if err != nil {
				t.Fatal(err)
			}
			result := out.Error + out.Notes + string(output)
			if out.ExitCode != tt.exitCode || !strings.Contains(result, tt.want) {
				t.Errorf("secrets check --decrypt: %+v, want exit %d and %q", out, tt.exitCode, tt.want)
			}
			if strings.Contains(result, "hunter2") {
				t.Errorf("the plaintext reached the agent: %s", result)
			}
		})
	}
}
