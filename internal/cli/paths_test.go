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
			if got, want := exitcode.From(err), exitcode.Usage; got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}
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
	if got, want := exitcode.From(err), exitcode.Usage; got != want {
		t.Errorf("exit code = %d, want %d", got, want)
	}
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
		if got, want := exitcode.From(err), exitcode.Usage; got != want {
			t.Errorf("exit code = %d, want %d", got, want)
		}
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
