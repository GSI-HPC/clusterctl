// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/clusterctl/internal/app"
	"github.com/GSI-HPC/clusterctl/internal/config"
	"github.com/GSI-HPC/clusterctl/internal/config/configtest"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
)

// TestAStateDirectoryOthersCanWriteIsRefused is report section 9.4: with
// HOME unset the state directory was a fixed path in /tmp that another user
// could create first with mode 0777, and login printed an ssh -F pointing
// into it while doctor called it ok. A command now refuses to run.
func TestAStateDirectoryOthersCanWriteIsRefused(t *testing.T) {
	for _, name := range []string{"state", "cache"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "clusterctl")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o777); err != nil {
				t.Fatal(err)
			}
			streams := func(s *app.Streams) {
				if name == "state" {
					s.StateDir = dir
				} else {
					s.CacheDir = dir
				}
			}

			h, err := run(t, harnessOptions{streams: streams}, "login", "--dry-run", "install", "--", "true")
			if err == nil {
				t.Fatalf("login ran with a %s directory anyone can write:\n%s", name, h.out)
			}
			wantCode(t, err, exitcode.Usage)
			if !strings.Contains(err.Error(), dir) {
				t.Errorf("the error does not name the directory: %v", err)
			}
			if strings.Contains(h.out.String(), "ssh") {
				t.Errorf("login printed a command:\n%s", h.out)
			}

			h, _ = run(t, harnessOptions{streams: streams}, "doctor")
			if !strings.Contains(h.out.String(), "failed") || !strings.Contains(h.out.String(), dir) {
				t.Errorf("doctor does not report the %s directory:\n%s", name, h.out)
			}
		})
	}
}

// TestNoStateDirectoryIsRefused keeps a command from running when no private
// directory is known, rather than falling back to one in the current or the
// temporary directory.
func TestNoStateDirectoryIsRefused(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "relative")
	_, err := run(t, harnessOptions{streams: func(s *app.Streams) { s.StateDir = "" }}, "config", "validate")
	if err == nil {
		t.Fatal("a command ran without a state directory")
	}
	wantCode(t, err, exitcode.Usage)
	if !strings.Contains(err.Error(), "XDG_STATE_HOME") {
		t.Errorf("the error does not say what to set: %v", err)
	}
}

// TestAMisspelledConfigIsReported is report section 9.5: a file named with
// --config or CLUSTERCTL_CONFIG that did not exist was skipped, and its
// currentContext with it, so the command resolved to the site's default
// context and exited 0.
func TestAMisspelledConfigIsReported(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "test-ctx.yaml")
	if err := os.WriteFile(good, []byte("apiVersion: clusterctl/v1alpha1\nkind: Config\ncurrentContext: cluster2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := run(t, harnessOptions{config: []string{good}}, "config", "validate")
	if err != nil {
		t.Fatalf("config validate failed: %v", err)
	}
	if !strings.Contains(h.out.String(), "context cluster2 resolves") {
		t.Fatalf("the context file is not read:\n%s", h.out)
	}

	misspelled := filepath.Join(dir, "test-ctx.yml")
	check := func(t *testing.T, h *harness, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("a missing configuration was skipped:\n%s", h.out)
		}
		wantCode(t, err, exitcode.Usage)
		if !strings.Contains(err.Error(), misspelled) {
			t.Errorf("the error does not name the missing file: %v", err)
		}
	}
	t.Run("--config", func(t *testing.T) {
		h, err := run(t, harnessOptions{config: []string{misspelled}}, "config", "validate")
		check(t, h, err)
	})
	t.Run(config.EnvConfig, func(t *testing.T) {
		t.Setenv(config.EnvConfig, exampleDir+string(os.PathListSeparator)+misspelled)
		h, err := run(t, harnessOptions{bare: true}, "bmc", "power", "off", "-n", "exe0001", "--dry-run")
		check(t, h, err)
	})
}

// copyExampleWithModes copies the example configuration into a new
// directory with the given mode, its files with fileMode.
func copyExampleWithModes(t *testing.T, mode, fileMode os.FileMode) string {
	t.Helper()
	dir := configtest.CopyDir(t, exampleDir)
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if err := os.Chmod(filepath.Join(dir, item.Name()), fileMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestConfigurationOthersCanWriteIsRefused is report section 9.7: the
// configuration names programs clusterctl runs, and it was read from files
// and directories anyone could write.
func TestConfigurationOthersCanWriteIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mode, fileMode os.FileMode
	}{
		{"directory anyone can write", 0o777, 0o644},
		{"directory the group can write", 0o775, 0o644},
		{"file anyone can write", 0o755, 0o646},
		{"file the group can write", 0o755, 0o664},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyExampleWithModes(t, tc.mode, tc.fileMode)
			for _, entry := range []string{dir, filepath.Join(dir, "site.yaml")} {
				// A file nobody else can write is refused too
				// when it is in a directory others can write,
				// who could swap it for another.
				_, err := run(t, harnessOptions{bare: true, config: []string{entry}}, "config", "validate")
				if err == nil {
					t.Fatalf("configuration in %s was read", entry)
				}
				wantCode(t, err, exitcode.Usage)
				if !strings.Contains(err.Error(), "chmod go-w") {
					t.Errorf("the error does not say how to fix it: %v", err)
				}
			}
		})
	}

	// The same files with nobody else able to write them are read.
	dir := copyExampleWithModes(t, 0o755, 0o644)
	if _, err := run(t, harnessOptions{bare: true, config: []string{dir}}, "config", "validate"); err != nil {
		t.Errorf("a private copy of the example was refused: %v", err)
	}
}

// TestAnotherUsersConfigurationIsRefused is the case the report showed:
// root read a document another user had added, and doctor ran the program it
// named as ssh.binary.
func TestAnotherUsersConfigurationIsRefused(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only root can give a file to another user")
	}
	dir := copyExampleWithModes(t, 0o755, 0o644)
	marker := filepath.Join(t.TempDir(), "ran")
	program := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(dir, "zz-extra.yaml")
	doc := "apiVersion: clusterctl/v1alpha1\nkind: Config\ncurrentContext: cluster1\ncontexts:\n" +
		"  - name: cluster1\n    cluster: cluster1\n    overrides:\n      ssh.binary: " + program + "\n"
	if err := os.WriteFile(extra, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(extra, 65534, 65534); err != nil {
		t.Fatal(err)
	}

	h, _ := run(t, harnessOptions{bare: true, config: []string{dir}}, "doctor")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("doctor ran a program another user's document named")
	}
	if !strings.Contains(h.out.String(), "uid 65534") {
		t.Errorf("doctor does not say whose file was refused:\n%s", h.out)
	}
}

// TestConfigInitRefusesADirectoryOthersCanWrite is the other half of 9.7:
// config init wrote into an empty directory another user had created with
// mode 0777, and told root to export CLUSTERCTL_CONFIG pointing at it.
func TestConfigInitRefusesADirectoryOthersCanWrite(t *testing.T) {
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{open, filepath.Join(open, "cfg"), filepath.Join(open, "a", "cfg")} {
		h, err := run(t, harnessOptions{}, "config", "init", dir)
		if err == nil {
			t.Fatalf("config init wrote into %s:\n%s", dir, h.out)
		}
		wantCode(t, err, exitcode.Usage)
		if strings.Contains(h.out.String(), "export") {
			t.Errorf("config init said to read %s:\n%s", dir, h.out)
		}
	}
	if items, _ := os.ReadDir(open); len(items) != 0 {
		t.Errorf("a refused config init left %d entries behind", len(items))
	}

	// A directory in /tmp, which anyone can write but nobody can take
	// another user's file from, is fine.
	sticky := filepath.Join(t.TempDir(), "sticky")
	if err := os.Mkdir(sticky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, harnessOptions{}, "config", "init", filepath.Join(sticky, "cfg")); err != nil {
		t.Errorf("config init below a sticky directory failed: %v", err)
	}
}

func TestConfigInitRefusesAnotherUsersDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only root can give a directory to another user")
	}
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, harnessOptions{}, "config", "init", dir)
	if err == nil {
		t.Fatal("config init wrote into another user's directory")
	}
	wantCode(t, err, exitcode.Usage)
}
