// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GSI-HPC/clusterctl/internal/fileutil"
)

// Environment variables that steer where configuration and state live, and
// stand in for global flags that are not given.
const (
	// EnvConfig lists the configuration files or directories to read, most
	// general first, separated the way PATH is. It replaces the search path
	// entirely.
	EnvConfig = "CLUSTERCTL_CONFIG"
	// EnvContext selects the context, as --context does.
	EnvContext = "CLUSTERCTL_CONTEXT"
	// EnvNodes holds the node set commands act on when -n is not given.
	EnvNodes = "CLUSTERCTL_NODES"
	// EnvProgress chooses how progress is shown when --progress is not
	// given.
	EnvProgress = "CLUSTERCTL_PROGRESS"
	// EnvProgressLog names the file the progress events of a command are
	// appended to when --progress-log is not given.
	EnvProgressLog = "CLUSTERCTL_PROGRESS_LOG"
)

// listSeparator separates the entries of CLUSTERCTL_CONFIG.
func listSeparator() string {
	if runtime.GOOS == "windows" {
		return ";"
	}
	return ":"
}

// ConfigDirs returns the directories searched for configuration when
// CLUSTERCTL_CONFIG is not set, most specific last.
func ConfigDirs() []string {
	var dirs []string
	if dir, err := UserConfigDir(); err == nil {
		dirs = append(dirs, dir)
	}
	return append([]string{"/etc/clusterctl"}, dirs...)
}

// UserConfigDir returns the administrator's own configuration directory,
// usually ~/.config/clusterctl, which is searched after /etc/clusterctl.
func UserConfigDir() (string, error) {
	home, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("the configuration directory %q is not an absolute path", home)
	}
	return filepath.Join(home, "clusterctl"), nil
}

// StateDir returns the directory holding the generated ssh configuration,
// the control sockets and the BMC certificate pins.
func StateDir() (string, error) {
	return xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"))
}

// CacheDir returns the directory holding fetched copies of remote files and
// resolved group listings.
func CacheDir() (string, error) {
	return xdgDir("XDG_CACHE_HOME", ".cache")
}

// xdgDir returns clusterctl's directory under the base directory an XDG
// variable names, or under the home directory when it is not set. A relative
// value is ignored, as the XDG specification requires.
//
// Without either there is no directory: a fixed one in /tmp would be shared
// by every user of the host, and whoever made it first would choose the ssh
// configuration and certificate pins clusterctl trusts.
func xdgDir(variable, fallback string) (string, error) {
	if dir := os.Getenv(variable); filepath.IsAbs(dir) {
		return filepath.Join(dir, "clusterctl"), nil
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return filepath.Join(home, fallback, "clusterctl"), nil
	}
	return "", fmt.Errorf("HOME is not set and %s is not an absolute path; set one of them", variable)
}

// ExpandPath expands a leading ~ and makes a relative path absolute against
// base, which is the directory the value was configured in.
func ExpandPath(path, base string) string {
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	if filepath.IsAbs(path) || base == "" {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// SearchPath returns the files to read, in the order they are layered. The
// entries of CLUSTERCTL_CONFIG have to exist; the configuration directories
// searched without it may not.
func SearchPath(env func(string) string) ([]string, error) {
	if entries := EnvEntries(env); entries != nil {
		return ExpandEntries(entries)
	}
	return ExpandSearchDirs(ConfigDirs())
}

// ExpandSearchDirs is ExpandEntries for the directories of the built-in
// search path, which a site may or may not use: one that does not exist is
// skipped.
func ExpandSearchDirs(dirs []string) ([]string, error) {
	return expandEntries(dirs, true)
}

// SearchEntries returns the files and directories configuration is read from
// when --config is not given: the entries of CLUSTERCTL_CONFIG when it is
// set, the configuration directories otherwise.
func SearchEntries(env func(string) string) []string {
	if entries := EnvEntries(env); entries != nil {
		return entries
	}
	return ConfigDirs()
}

// EnvEntries returns the files and directories CLUSTERCTL_CONFIG names, or
// nil when it is not set.
func EnvEntries(env func(string) string) []string {
	if env == nil {
		env = os.Getenv
	}
	if list := env(EnvConfig); list != "" {
		return strings.Split(list, listSeparator())
	}
	return nil
}

// ExpandEntries turns files and directories into the list of files to read,
// in the order they are layered. A directory contributes its YAML files in
// name order.
//
// Every entry has to exist. These are the ones named with --config or
// CLUSTERCTL_CONFIG, and one that is not there is most likely misspelled:
// leaving it out would take its currentContext and its overrides with it,
// and the command would run against another context without a word.
func ExpandEntries(entries []string) ([]string, error) {
	return expandEntries(entries, false)
}

// expandEntries is ExpandEntries, skipping an entry that does not exist
// when skipMissing is set.
func expandEntries(entries []string, skipMissing bool) ([]string, error) {
	var files []string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		entry = ExpandPath(entry, "")
		info, err := os.Stat(entry)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if skipMissing {
					continue
				}
				return nil, &fs.PathError{Op: "reading configuration", Path: entry, Err: fs.ErrNotExist}
			}
			return nil, err
		}
		if !info.IsDir() {
			if err := checkTrusted(entry); err != nil {
				return nil, err
			}
			if err := fileutil.CheckTrustedParent(entry); err != nil {
				return nil, fmt.Errorf("refusing to read configuration: %w", err)
			}
			files = append(files, entry)
			continue
		}
		if err := checkTrusted(entry); err != nil {
			return nil, err
		}
		matches, err := yamlFilesIn(entry)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			if err := checkTrusted(match); err != nil {
				return nil, err
			}
		}
		files = append(files, matches...)
	}
	return files, nil
}

// checkTrusted refuses a configuration file or directory someone other than
// this user or root could have written. The configuration names programs
// clusterctl runs, ssh.binary and a password command among them, so whoever
// can write it runs their programs with the administrator's credentials.
func checkTrusted(path string) error {
	if err := fileutil.CheckTrusted(path); err != nil {
		return fmt.Errorf("refusing to read configuration: %w", err)
	}
	return nil
}

// yamlFilesIn lists the YAML files of a directory in name order, leaving out
// hidden ones.
func yamlFilesIn(dir string) ([]string, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, item := range items {
		// A hidden file is not configuration: .sops.yaml holds the rules
		// sops encrypts with, and an editor keeps its swap files there.
		if item.IsDir() || strings.HasPrefix(item.Name(), ".") {
			continue
		}
		switch strings.ToLower(filepath.Ext(item.Name())) {
		case ".yaml", ".yml":
			out = append(out, filepath.Join(dir, item.Name()))
		}
	}
	return out, nil
}
