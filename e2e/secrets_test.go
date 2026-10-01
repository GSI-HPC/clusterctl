// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

//go:build e2e

package e2e

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// TestSecretsPushWritesEachFileExactly pushes files that hold every kind of
// byte onto the workers, all of a node's in one session, and reads them back
// on each node past clusterctl: what arrives is what was sealed, with the
// mode asked for, and a file that cannot be written, its directory being a
// file, fails alone and leaves the next to be written.
func TestSecretsPushWritesEachFileExactly(t *testing.T) {
	const remote = "/tmp/clusterctl-e2e-secrets"
	big := make([]byte, 64<<10)
	for i := range big {
		big[i] = byte(i*31 + i>>8)
	}
	files := []struct {
		target  string
		content []byte
		mode    string
	}{
		{remote + "/text", []byte("line one\nline two\n"), "0640"},
		{remote + "/blocked/key", []byte("never written"), ""},
		{remote + "/sub/binary", []byte{0, '\n', 0xff, '\r', '\\', '\t', 0}, "0400"},
		{remote + "/empty", nil, ""},
		{remote + "/big", big, ""},
	}
	t.Cleanup(func() {
		for _, node := range workers {
			_, _ = dockerExec(node, "rm", "-rf", remote)
		}
	})
	for _, node := range workers {
		onNode(t, node, "sh", "-c", "mkdir -p "+remote+" && : > "+remote+"/blocked")
	}

	dir := t.TempDir()
	var refs []map[string]string
	for i, f := range files {
		sealed := filepath.Join(dir, fmt.Sprintf("%d.age", i))
		seal(t, sealed, f.content)
		ref := map[string]string{"target": f.target, "source": sealed}
		if f.mode != "" {
			ref["mode"] = f.mode
		}
		refs = append(refs, ref)
	}
	value, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}

	r := clusterctl(t, "--set", "services.cinc.secrets="+string(value), "secrets", "push", "-n", "worker-[0-2]", "-y")
	r.wantCode(t, 1)
	status := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			t.Fatalf("a row of the table is short: %q\n%s", line, r)
		}
		status[fields[0]+" "+fields[1]] = strings.Join(fields[2:], " ")
	}
	for _, node := range workers {
		for i, f := range files {
			got := status[node+" "+f.target]
			if i == 1 {
				if !strings.HasPrefix(got, "failed: mkdir: ") {
					t.Errorf("%s %s: %q, want it failed by mkdir\n%s", node, f.target, got, r)
				}
				continue
			}
			if got != "written" {
				t.Errorf("%s %s: %q, want it written\n%s", node, f.target, got, r)
				continue
			}
			encoded := strings.TrimSpace(onNode(t, node, "base64", "-w0", f.target))
			if content, err := base64.StdEncoding.DecodeString(encoded); err != nil || !bytes.Equal(content, f.content) {
				t.Errorf("%s holds %d bytes at %s (%v), want the %d that were sealed", node, len(content), f.target, err, len(f.content))
			}
			mode := strings.TrimLeft(cmp.Or(f.mode, "0600"), "0")
			if got := strings.TrimSpace(onNode(t, node, "stat", "-c", "%a", f.target)); got != mode {
				t.Errorf("%s has %s at mode %s, want %s", node, f.target, got, mode)
			}
		}
	}
}

// seal encrypts content with age to the suite's identity, into path.
func seal(t *testing.T, path string, content []byte) {
	t.Helper()
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, sealed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
