// SPDX-License-Identifier: LGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Environment variables that steer where configuration and state live.
const (
	// EnvConfig lists the configuration files or directories to read, most
	// general first, separated the way PATH is. It replaces the search path
	// entirely.
	EnvConfig = "CLUSTERCTL_CONFIG"
	// EnvContext selects the context, as --context does.
	EnvContext = "CLUSTERCTL_CONTEXT"
	// EnvNodes holds the node set commands act on when -n is not given.
	EnvNodes = "CLUSTERCTL_NODES"
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
	if home, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "clusterctl"))
	}
	return append([]string{"/etc/clusterctl"}, dirs...)
}

// StateDir returns the directory holding the generated ssh configuration,
// the control sockets and the BMC certificate pins.
func StateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "clusterctl")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "clusterctl")
	}
	return filepath.Join(os.TempDir(), "clusterctl")
}

// CacheDir returns the directory holding fetched copies of remote files and
// resolved group listings.
func CacheDir() string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "clusterctl")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "clusterctl")
	}
	return filepath.Join(os.TempDir(), "clusterctl-cache")
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

// SearchPath returns the files to read, in the order they are layered.
func SearchPath(env func(string) string) ([]string, error) {
	if env == nil {
		env = os.Getenv
	}
	var entries []string
	if list := env(EnvConfig); list != "" {
		entries = strings.Split(list, listSeparator())
	} else {
		entries = ConfigDirs()
	}
	return ExpandEntries(entries)
}

// ExpandEntries turns files and directories into the list of files to read,
// in the order they are layered. A directory contributes its YAML files in
// name order; an entry that does not exist is skipped, because the search
// path names places a site may or may not use.
func ExpandEntries(entries []string) ([]string, error) {
	var files []string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		entry = ExpandPath(entry, "")
		info, err := os.Stat(entry)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, entry)
			continue
		}
		matches, err := yamlFilesIn(entry)
		if err != nil {
			return nil, err
		}
		files = append(files, matches...)
	}
	return files, nil
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
