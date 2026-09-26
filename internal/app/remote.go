// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/GSI-HPC/clusterctl/internal/apis/v1alpha1"
	"github.com/GSI-HPC/clusterctl/internal/exitcode"
	"github.com/GSI-HPC/clusterctl/internal/fileutil"
	"github.com/GSI-HPC/clusterctl/internal/progress"
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
// run would. The read is reported as a hidden call, with Cache saying whether
// the cached copy answered it or the host was asked.
func (a *App) RemoteFile(ctx context.Context, role, path string, ttl time.Duration) (_ []byte, err error) {
	target, err := a.Role(role)
	if err != nil {
		return nil, err
	}
	ctx, read := progress.Start(ctx, progress.KindCall, "read "+path, progress.WithFlags(progress.Hidden),
		progress.Role(role), progress.Host(target.Host))
	cache := "miss"
	defer func() { read.End(err, progress.Cache(cache)) }()
	// The cache directory is shared by every configuration, context and the
	// MCP server of one user, so the key holds everything that decides
	// which file is read: the site, cluster and context, the host with its
	// account and the way it is reached, and the path.
	key := struct {
		Scope  string            `json:"scope"`
		Target transport.Target  `json:"target"`
		Host   v1alpha1.HostRole `json:"host"`
		Path   string            `json:"path"`
	}{a.cacheScope(), target, a.Spec.Hosts[target.Role], path}
	dir := filepath.Join(a.CacheDir, "remote")
	if data, ok := fileutil.ReadCache(dir, key, ttl); ok && ttl > 0 {
		cache = "hit"
		return data, nil
	}

	result, err := a.ReadOnRole(ctx, role, transport.Request{Argv: []string{"cat", path}})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	data := []byte(result.Stdout)
	if ttl > 0 {
		fileutil.WriteCache(dir, key, data)
	}
	return data, nil
}

// cacheScope names the site, cluster and context a command runs against,
// which is what keeps one cluster's cached answers from another's.
func (a *App) cacheScope() string {
	scope, _ := json.Marshal([]string{
		a.Resolved.SiteName, a.Resolved.ClusterName, a.Resolved.Context.Name,
	})
	return string(scope)
}

// Collect fills in what a request whose output is collected, rather than
// shown on a terminal, leaves out: no terminal, and the configured command
// timeout unless it sets one.
func (a *App) Collect(req transport.Request) transport.Request {
	req.TTY = transport.TTYNone
	req.Timeout = cmp.Or(req.Timeout, a.Timeout().Get())
	return req
}

// RunOnRole runs one command on an infrastructure host and returns the
// result, failing the command when it did not succeed. The request is
// completed by Collect.
func (a *App) RunOnRole(ctx context.Context, role string, req transport.Request) (*transport.Result, error) {
	return a.runOnRole(ctx, a.Runner, role, req)
}

// ReadOnRole is RunOnRole for a command that only reads. It runs through
// ReadRunner, so a dry run reaches the host and sees what the real run
// would.
func (a *App) ReadOnRole(ctx context.Context, role string, req transport.Request) (*transport.Result, error) {
	return a.runOnRole(ctx, a.ReadRunner, role, req)
}

func (a *App) runOnRole(ctx context.Context, runner transport.Runner, role string, req transport.Request) (*transport.Result, error) {
	target, err := a.Role(role)
	if err != nil {
		return nil, err
	}
	result, err := runner.Run(ctx, target, a.Collect(req))
	if err != nil {
		// An interrupt is left as it is: report turns it into 130.
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, exitcode.Default(exitcode.Transport, err)
	}
	return result, result.Check("")
}
