// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
func (a *App) RemoteFile(ctx context.Context, role, path string, ttl time.Duration) ([]byte, error) {
	cache := a.remoteCachePath(role, path)
	if ttl > 0 {
		if info, err := os.Stat(cache); err == nil && time.Since(info.ModTime()) < ttl {
			if data, err := os.ReadFile(cache); err == nil && len(data) > 0 {
				return data, nil
			}
		}
	}

	target, err := a.Role(role)
	if err != nil {
		return nil, err
	}
	result, err := a.Runner.Run(ctx, target, transport.Request{
		Argv:    []string{"cat", path},
		Timeout: a.Timeout().Get(),
		TTY:     transport.TTYNone,
	})
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Transport, err)
	}
	if result.Failed() {
		if result.Err != nil {
			return nil, exitcode.Wrap(exitcode.Transport, result.Err)
		}
		return nil, exitcode.Errorf(exitcode.TargetFailed,
			"reading %s on %s: %s", path, target, result.Stderr)
	}

	data := []byte(result.Stdout)
	if ttl > 0 && len(data) > 0 {
		// A cache that cannot be written is not worth failing the command
		// over; the next call simply fetches again.
		_ = fileutil.WriteAtomic(cache, data, 0o600)
	}
	return data, nil
}

func (a *App) remoteCachePath(role, path string) string {
	sum := sha256.Sum256([]byte(role + ":" + path))
	return filepath.Join(a.CacheDir, "remote", fmt.Sprintf("%s-%s", role, hex.EncodeToString(sum[:8])))
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
		return nil, exitcode.Wrap(exitcode.Transport, err)
	}
	if result.Failed() {
		if result.Err != nil {
			return result, exitcode.Wrap(exitcode.Transport, result.Err)
		}
		return result, exitcode.Errorf(exitcode.TargetFailed, "%s: %s",
			target, firstNonEmptyLine(result.Stderr, result.Stdout))
	}
	return result, nil
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
