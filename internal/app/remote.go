// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/transport"
)

// RemoteFile reads a file from an infrastructure host, reusing a cached copy
// while it is younger than ttl.
//
// A configuration such as dhcpd.conf is read once per command and often
// several times in a row, and fetching it again for each node turned a node
// set query into one connection per node.
//
// The file is read through ReadRunner, so a dry run sees the same file the real
// run would.
func (a *App) RemoteFile(ctx context.Context, role, path string, ttl time.Duration) ([]byte, error) {
	target, err := a.Role(role)
	if err != nil {
		return nil, err
	}
	cache := a.remoteCachePath(target, path)
	if ttl > 0 {
		if info, err := os.Stat(cache); err == nil && time.Since(info.ModTime()) < ttl {
			if data, err := os.ReadFile(cache); err == nil && len(data) > 0 {
				return data, nil
			}
		}
	}

	result, err := a.ReadRunner.Run(ctx, target, transport.Request{
		Argv:    []string{"cat", path},
		Timeout: a.Timeout().Get(),
		TTY:     transport.TTYNone,
	})
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Transport, err)
	}
	if result.Failed() {
		// ssh's own failure arrives coded as a transport failure. Anything
		// else happened on a host that answered, and what it said is the
		// reason.
		var coded *exitcode.Error
		if errors.As(result.Err, &coded) {
			return nil, result.Err
		}
		// The message came from the host, so it is quoted: a control
		// character in it cannot rewrite the terminal it is printed on.
		return nil, exitcode.Errorf(exitcode.TargetFailed, "reading %s on %s: %q",
			path, target, firstNonEmptyLine(result.Stderr, result.Stdout))
	}

	data := []byte(result.Stdout)
	if ttl > 0 && len(data) > 0 {
		// A cache that cannot be written is not worth failing the command
		// over; the next call simply fetches again.
		_ = fileutil.WriteAtomic(cache, data, 0o600)
	}
	return data, nil
}

// remoteCachePath names the cached copy of one file on one host.
//
// The cache directory is shared by every configuration, context and the MCP
// server of one user, so the key holds everything that decides which file
// is read: the site, cluster and context, the host with its account and the
// way it is reached, and the path.
func (a *App) remoteCachePath(target transport.Target, path string) string {
	key, _ := json.Marshal(struct {
		Scope  string            `json:"scope"`
		Target transport.Target  `json:"target"`
		Host   v1alpha1.HostRole `json:"host"`
		Path   string            `json:"path"`
	}{a.cacheScope(), target, a.Spec.Hosts[target.Role], path})
	sum := sha256.Sum256(key)
	return filepath.Join(a.CacheDir, "remote", hex.EncodeToString(sum[:]))
}

// cacheScope names the site, cluster and context a command runs against,
// which is what keeps one cluster's cached answers from another's.
func (a *App) cacheScope() string {
	scope, _ := json.Marshal([]string{
		a.Resolved.SiteName, a.Resolved.ClusterName, a.Resolved.Context.Name,
	})
	return string(scope)
}

// RunOnRole runs one command on an infrastructure host and returns the
// result, failing the command when it did not succeed.
func (a *App) RunOnRole(ctx context.Context, role string, req transport.Request) (*transport.Result, error) {
	target, err := a.Role(role)
	if err != nil {
		return nil, err
	}
	result, err := a.Runner.Run(ctx, target, req)
	if err != nil {
		if hasCode(err) {
			return nil, err
		}
		return nil, exitcode.Wrap(exitcode.Transport, err)
	}
	if result.Failed() {
		// ssh's own failure arrives coded as a transport failure, and an
		// interrupt as one. Anything else happened on a host that answered,
		// and what it said is the reason.
		if hasCode(result.Err) {
			return result, result.Err
		}
		if strings.TrimSpace(result.Stderr+result.Stdout) == "" {
			return result, exitcode.Errorf(exitcode.TargetFailed, "%s: command exited %d", target, result.ExitCode)
		}
		// The message came from the host, so it is quoted: a control
		// character in it cannot rewrite the terminal it is printed on.
		return result, exitcode.Errorf(exitcode.TargetFailed, "%s: exit %d: %q",
			target, result.ExitCode, firstNonEmptyLine(result.Stderr, result.Stdout))
	}
	return result, nil
}

// hasCode reports whether an error already says which exit code it asks for,
// or is an interrupt, which report turns into 130.
func hasCode(err error) bool {
	var coded *exitcode.Error
	return errors.As(err, &coded) || errors.Is(err, context.Canceled)
}

func firstNonEmptyLine(candidates ...string) string {
	for _, c := range candidates {
		for _, line := range splitLines(c) {
			if line != "" {
				return line
			}
		}
	}
	return "the command failed without saying why"
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, trimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, trimSpace(s[start:]))
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
