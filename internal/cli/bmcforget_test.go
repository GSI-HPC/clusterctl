// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// pinFile writes a pin store with a pin for each host and returns its path
// and the --set that points the configuration at it.
func pinFile(t *testing.T, hosts ...string) (string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pins")
	var b strings.Builder
	for i, host := range hosts {
		b.WriteString(host + " sha256:" + strings.Repeat(string(rune('a'+i)), 64) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, []string{"--set", "bmc.redfish.pinStore=" + path}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// bmc forget removed the pin in a dry run, never asked, never checked for a
// protected host, and said it forgot a pin it had not found.
func TestBMCForgetGoesThroughTheGate(t *testing.T) {
	isolateHome(t)
	hosts := []string{"exe0007.mgmt.hpc.example.org", "exe0008.mgmt.hpc.example.org", "wlm01.mgmt.hpc.example.org"}
	path, set := pinFile(t, hosts...)
	before := readFile(t, path)

	h, err := run(t, harnessOptions{}, append(set, "--dry-run", "bmc", "forget", "exe0007")...)
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("the dry run changed the pin store:\n%s", got)
	}
	if out := h.errOut.String() + h.out.String(); !strings.Contains(out, "sha256:aaaa") {
		t.Errorf("the dry run does not show the fingerprint it would drop:\n%s", out)
	}

	// Without a terminal and without -y there is nobody to ask.
	_, err = run(t, harnessOptions{}, append(set, "bmc", "forget", "exe0007")...)
	if got := exitcode.From(err); got != exitcode.Usage {
		t.Errorf("forget without a confirmation: exit code %d, want %d (%v)", got, exitcode.Usage, err)
	}
	// A protected host is refused, by either name.
	for _, arg := range []string{"wlm01", "wlm01.mgmt.hpc.example.org"} {
		_, err = run(t, harnessOptions{}, append(set, "bmc", "forget", "-y", arg)...)
		if got := exitcode.From(err); got != exitcode.Usage {
			t.Errorf("forget of the protected host %s: exit code %d, want %d (%v)", arg, got, exitcode.Usage, err)
		}
	}
	if got := readFile(t, path); got != before {
		t.Errorf("a refused forget changed the pin store:\n%s", got)
	}

	// exe7 is exe0007, and the BMC host the mismatch error names works too.
	for _, arg := range []string{"exe7", "exe0008.mgmt.hpc.example.org"} {
		h, err = run(t, harnessOptions{}, append(set, "bmc", "forget", "-y", arg)...)
		if err != nil {
			t.Fatalf("forget %s failed: %v", arg, err)
		}
		if !strings.Contains(h.errOut.String(), "forgot the certificate of") {
			t.Errorf("forget %s does not say what it dropped:\n%s", arg, h.errOut)
		}
	}
	if got := readFile(t, path); strings.Contains(got, "exe0007") || strings.Contains(got, "exe0008") ||
		!strings.Contains(got, "wlm01") {
		t.Errorf("pin store after forgetting exe0007 and exe0008:\n%s", got)
	}

	// Nothing recorded is said, not reported as forgotten.
	h, err = run(t, harnessOptions{}, append(set, "bmc", "forget", "-y", "exe0009")...)
	if err == nil {
		t.Errorf("forgetting a pin that is not recorded succeeded:\n%s%s", h.out, h.errOut)
	}
	if strings.Contains(h.errOut.String()+h.out.String(), "forgot") {
		t.Errorf("claims to have forgotten a pin that was not there:\n%s%s", h.out, h.errOut)
	}
}
