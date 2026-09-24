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

// exampleCopy copies the example configuration into a temporary directory
// and replaces text in its site document, so that a test can change one
// setting and still run against everything else the example defines.
func exampleCopy(t *testing.T, replace ...string) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(exampleDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if e.Name() == "site.yaml" {
			text := string(data)
			for i := 0; i+1 < len(replace); i += 2 {
				if !strings.Contains(text, replace[i]) {
					t.Fatalf("site.yaml has no %q to replace", replace[i])
				}
				text = strings.Replace(text, replace[i], replace[i+1], 1)
			}
			data = []byte(text)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The shell toolkit compared names as they were typed, and so did the first
// version of the gate: wlm01 was refused, and its host name, its service
// processor, its address and WLM01 all reached it.
func TestProtectedHostIsRefusedUnderEverySpelling(t *testing.T) {
	spellings := []string{
		"wlm01",
		"wlm01.hpc.example.org",
		"wlm01.hpc.example.org.",
		"wlm01.",
		"WLM01",
		"Wlm01.HPC.example.org",
		"wlm01.example.org",
		"wlm01.mgmt.hpc.example.org",
		"10.0.1.1",
		"exe0001,wlm01.hpc.example.org",
	}
	for _, name := range spellings {
		t.Run(name, func(t *testing.T) {
			h, err := run(t, harnessOptions{}, "exec", "--dry-run", "-n", name, "--", "systemctl", "poweroff")
			if err == nil {
				t.Fatalf("exec -n %s was let through:\n%s", name, h.errOut)
			}
			if !strings.Contains(err.Error(), "protected host wlm01") {
				t.Errorf("error = %v, want it to name wlm01 as protected", err)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}

			// A power action may also be stopped earlier, by the naming
			// rules refusing a domain they do not give wlm01; either way
			// it is refused and nothing is sent.
			h, err = run(t, harnessOptions{}, "bmc", "power", "off", "-n", name, "--dry-run")
			if err == nil {
				t.Fatalf("power off -n %s was let through:\n%s", name, h.errOut)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}
			if !strings.Contains(err.Error(), "protected host wlm01") &&
				!strings.Contains(err.Error(), "is not the host name the naming rules give wlm01") {
				t.Errorf("error = %v, want wlm01 refused", err)
			}
		})
	}
}

func TestProtectedHostIsNotReachedUnderAnotherSpelling(t *testing.T) {
	tests := [][]string{
		{"bmc", "power", "off", "--ipmi", "-y", "-n", "wlm01.hpc.example.org"},
		{"exec", "--confirm", "-y", "-n", "wlm01.hpc.example.org", "--", "systemctl", "poweroff"},
		{"slurm", "node", "drain", "maintenance", "-y", "-n", "WLM01."},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, err := run(t, harnessOptions{}, args...)
			// drain asks Slurm about the nodes first, and Slurm here knows
			// none of them; any refusal will do, as long as nothing is sent.
			if err == nil {
				t.Fatal("the command went ahead")
			}
			if args[0] != "slurm" && !strings.Contains(err.Error(), "protected host wlm01") {
				t.Errorf("error = %v, want wlm01 refused as protected", err)
			}
			for _, cmd := range h.recorder.Commands() {
				// Slurm may be asked about the node; nothing may be sent to it.
				if strings.Contains(cmd, "wlm01") && !strings.Contains(cmd, "sinfo") {
					t.Errorf("sent %q to the protected host", cmd)
				}
			}
		})
	}
}

// The documented form of an entry is the inventory name, but the hosts roles
// of the same document use host names, and nothing said the other form
// protected nothing.
func TestProtectedHostsEntryProtectsTheMachineHoweverItIsWritten(t *testing.T) {
	for _, entry := range []string{"wlm01.hpc.example.org", "WLM01", "wlm01.", "wlm1", "10.0.1.1", "wlm01.mgmt.hpc.example.org"} {
		t.Run(entry, func(t *testing.T) {
			dir := exampleCopy(t, "      - wlm01\n", "      - "+entry+"\n")
			if _, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate"); err != nil {
				t.Fatalf("config validate refused %q: %v", entry, err)
			}
			_, err := run(t, harnessOptions{bare: true, config: []string{dir}},
				"bmc", "power", "off", "-n", "wlm01", "--dry-run")
			if err == nil || !strings.Contains(err.Error(), "protected host wlm01") {
				t.Errorf("error = %v, want wlm01 refused as protected", err)
			}
		})
	}
}

func TestProtectedHostsEntryThatNamesNoKnownNodeIsRefused(t *testing.T) {
	for _, entry := range []string{"wlm02", "wlm01.other.example.org", "wlm0[1-2]"} {
		t.Run(entry, func(t *testing.T) {
			dir := exampleCopy(t, "      - wlm01\n", "      - "+entry+"\n")
			_, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate")
			if err == nil {
				t.Fatalf("config validate accepted the protected host %q, which the inventory does not know", entry)
			}
			if !strings.Contains(err.Error(), "safety.protectedHosts") {
				t.Errorf("error = %v, want it to name the setting", err)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}
			// A command that changes something refuses too, rather than
			// running with less protection than the site wrote down.
			if _, err := run(t, harnessOptions{bare: true, config: []string{dir}},
				"bmc", "power", "off", "-n", "exe0002", "--dry-run"); err == nil {
				t.Error("a change ran with a protected host entry that names no known node")
			}
		})
	}
}

// The access guide lists a group under protectedHosts, and every command,
// config validate included, failed because the gate parsed it without the
// group sources.
func TestProtectedHostsEntryMayBeAGroup(t *testing.T) {
	dir := exampleCopy(t, "      - wlm01\n", "      - \"@inventory:wlm\"\n")
	if _, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate"); err != nil {
		t.Fatalf("config validate refused a group entry: %v", err)
	}
	if _, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "node", "select", "exe1"); err != nil {
		t.Fatalf("a read-only command failed: %v", err)
	}
	_, err := run(t, harnessOptions{bare: true, config: []string{dir}},
		"bmc", "power", "off", "-n", "wlm01.hpc.example.org", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "protected host wlm01") {
		t.Errorf("error = %v, want wlm01 refused as protected", err)
	}
}

func TestProtectedHostsGroupThatCannotBeResolvedRefusesChanges(t *testing.T) {
	dir := exampleCopy(t, "      - wlm01\n", "      - \"@inventory:nosuchgroup\"\n")
	if _, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate"); err == nil {
		t.Error("config validate accepted a protected group that cannot be resolved")
	}
	h, err := run(t, harnessOptions{bare: true, config: []string{dir}},
		"exec", "--confirm", "-y", "-n", "exe0002", "--", "true")
	if err == nil {
		t.Fatal("a change ran although the protected hosts could not be worked out")
	}
	if !strings.Contains(err.Error(), "safety.protectedHosts") {
		t.Errorf("error = %v, want it to name the setting", err)
	}
	if calls := h.recorder.Calls(); len(calls) != 0 {
		t.Errorf("sent %d commands, want none", len(calls))
	}
}

// A name the inventory does not know may be another spelling of a machine
// the gate should have recognised, so a change to it has to be forced.
func TestChangeToANodeTheInventoryDoesNotKnowNeedsForce(t *testing.T) {
	h, err := run(t, harnessOptions{}, "exec", "--confirm", "-y", "-n", "ghost1", "--", "true")
	if err == nil {
		t.Fatal("a change to ghost1, which the inventory does not know, was let through")
	}
	if !strings.Contains(err.Error(), "ghost1") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %v, want it to name ghost1 and the way out", err)
	}
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
	if calls := h.recorder.Calls(); len(calls) != 0 {
		t.Errorf("sent %d commands, want none", len(calls))
	}

	h, err = run(t, harnessOptions{}, "exec", "--confirm", "-y", "--force", "-n", "ghost1", "--", "true")
	if err != nil {
		t.Fatalf("--force should allow it: %v", err)
	}
	if calls := h.recorder.Calls(); len(calls) != 1 {
		t.Errorf("sent %d commands, want one", len(calls))
	}
}

// One machine named three ways was three targets: three resets at once to
// one service processor, and the command run three times on the node.
func TestOneMachineNamedTwiceIsOneTarget(t *testing.T) {
	names := "exe0001,exe0001.hpc.example.org,exe0001.,EXE1,10.0.2.1,exe0001.mgmt.hpc.example.org"

	h, err := run(t, harnessOptions{}, "node", "select", names)
	if err != nil {
		t.Fatalf("node select failed: %v", err)
	}
	if got := strings.TrimSpace(h.out.String()); got != "exe0001" {
		t.Errorf("node select = %q, want exe0001", got)
	}

	h, err = run(t, harnessOptions{}, "exec", "-y", "-n", names, "--", "uptime")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	if calls := h.recorder.Calls(); len(calls) != 1 {
		t.Errorf("exec ran %d times, want once: %v", len(calls), h.recorder.Commands())
	}

	// The Slurm job check is not what this is about.
	h, err = run(t, harnessOptions{}, "bmc", "power", "cycle", "-n", names, "--dry-run", "--lose-jobs")
	if err != nil {
		t.Fatalf("power cycle --dry-run failed: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "Would power cycle 1 host: exe0001") {
		t.Errorf("the preview does not count one host:\n%s", h.errOut)
	}
}

// safety.confirmAbove: 0 read as "always type the count" everywhere but in a
// Go comment, and did the opposite.
func TestConfirmAboveZeroAlwaysAsksForTheCount(t *testing.T) {
	h, err := run(t, harnessOptions{tty: true, stdin: "y\n"},
		"--set", "safety.confirmAbove=0", "exec", "-n", "exe[0001-0002]", "--confirm", "--", "systemctl", "poweroff")
	if err == nil {
		t.Fatal("a y was accepted where the count had to be typed")
	}
	if !strings.Contains(h.errOut.String(), "Type the number of hosts") {
		t.Errorf("the count was not asked for:\n%s", h.errOut)
	}
	if calls := h.recorder.Calls(); len(calls) != 0 {
		t.Errorf("sent %d commands without the count", len(calls))
	}

	h, err = run(t, harnessOptions{tty: true, stdin: "1\n"},
		"--set", "safety.confirmAbove=0", "exec", "-n", "exe0001", "--confirm", "--", "true")
	if err != nil {
		t.Fatalf("typing the count of one host should confirm: %v\n%s", err, h.errOut)
	}
}

func TestSafetyLimitsOutOfRangeAreRefused(t *testing.T) {
	tests := []struct{ from, to string }{
		{"confirmAbove: 8", "confirmAbove: -5"},
		{"powerOnBatch: 8", "powerOnBatch: 0"},
		{"powerOnBatch: 8", "powerOnBatch: -1"},
	}
	for _, tc := range tests {
		t.Run(tc.to, func(t *testing.T) {
			dir := exampleCopy(t, tc.from, tc.to)
			_, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate")
			if err == nil {
				t.Fatalf("config validate accepted %s", tc.to)
			}
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}
		})
	}
	for _, set := range []string{"safety.confirmAbove=-5", "safety.powerOnBatch=0"} {
		t.Run(set, func(t *testing.T) {
			if _, err := run(t, harnessOptions{}, "--set", set, "config", "validate"); err == nil {
				t.Errorf("--set %s was accepted", set)
			}
		})
	}
}

// A change to the accounting database names no node, so the inventory has
// nothing to say about it.
func TestAccountingChangeIsNotANodeTheInventoryLacks(t *testing.T) {
	rec := (&slurmCluster{}).recorder()
	h, err := run(t, harnessOptions{recorder: rec}, "slurm", "account", "add", "physics", "-y")
	if err != nil {
		t.Fatalf("slurm account add failed: %v", err)
	}
	var added bool
	for _, cmd := range h.recorder.Commands() {
		added = added || strings.Contains(cmd, "sacctmgr") && strings.Contains(cmd, "add")
	}
	if !added {
		t.Errorf("sent %v, want the sacctmgr call", h.recorder.Commands())
	}
}

// A role host or user that ssh would read as something else is refused when
// connecting; the schema says so before then, so that config validate
// reports it.
func TestHostRoleMustBeAHostAndAUserName(t *testing.T) {
	tests := []struct {
		from, to string
		valid    bool
	}{
		{"host: login.hpc.example.org", "host: login.hpc.example.org.", true},
		{"host: login.hpc.example.org", "host: 10.0.0.5", true},
		{"host: login.hpc.example.org", "host: \"fd00::5\"", true},
		{"host: login.hpc.example.org", "host: root@login.hpc.example.org", false},
		{"host: login.hpc.example.org", "host: login.hpc.example.org:2222", false},
		{"host: login.hpc.example.org", "host: -oProxyCommand=sh", false},
		{"host: login.hpc.example.org", "host: login_1.example.org", false},
		{"user: root", "user: alice_adm", true},
		{"user: root", "user: -oProxyCommand=sh", false},
		{"user: root", "user: alice@EXAMPLE.ORG", false},
	}
	for _, tc := range tests {
		t.Run(tc.to, func(t *testing.T) {
			dir := exampleCopy(t, tc.from, tc.to)
			_, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate")
			if tc.valid && err != nil {
				t.Errorf("config validate refused %s: %v", tc.to, err)
			}
			if !tc.valid && err == nil {
				t.Errorf("config validate accepted %s", tc.to)
			}
		})
	}
}
